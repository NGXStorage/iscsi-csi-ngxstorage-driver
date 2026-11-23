package helper

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// verifyRoundTripper adapts a func to http.RoundTripper for the fake backend.
type verifyRoundTripper func(*http.Request) (*http.Response, error)

func (f verifyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newVerifyTestReconciler builds a Reconciler whose SDK client points at a
// mock NGX API. failTransport, when non-nil, injects a transport error for
// matching requests before they reach the server.
func newVerifyTestReconciler(t *testing.T, timeout, interval time.Duration, failTransport func(*http.Request) bool, handler http.HandlerFunc) *Reconciler {
	t.Helper()
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default routing for SDK controller selection: cluster status and
		// pool list/detail. Operation-specific paths delegate to handler.
		switch {
		case strings.Contains(r.URL.Path, "/status/cluster"):
			w.Write([]byte(`{"connected":1,"status":"Master","peers":[]}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool"):
			w.Write([]byte(`[{"id":"pool1","name":"pool1","owner":"o"}]`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/pool/pool1"):
			w.Write([]byte(`{"id":"pool1","name":"pool1","owner":"o"}`))
		default:
			handler(w, r)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: wrap}}
	server.Start()
	t.Cleanup(server.Close)

	transport := verifyRoundTripper(func(req *http.Request) (*http.Response, error) {
		if failTransport != nil && failTransport(req) {
			return nil, fmt.Errorf("read tcp: EOF")
		}
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		return http.DefaultTransport.RoundTrip(req)
	})
	client, err := ngxstorage.NewClient(ngxstorage.Config{
		Controllers: []string{"127.0.0.1"},
		APIKey:      "test-key",
		PoolName:    "pool1",
		HTTPClient:  &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("ngxstorage.NewClient: %v", err)
	}
	return &Reconciler{Client: client, DeleteVerifyTimeout: timeout, DeleteVerifyInterval: interval}
}

const verifyLUNPresentBody = `{"id":"lun-1","name":"pvcvol1","size":1073741824,"protocols":{}}`

func writeVerifyLUNAbsent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"code":5011,"error":"not found"}`))
}

// TestVerifyLUNDeleted pins the bounded read-after-delete semantics: only an
// authoritative NotFound proves absence; a present LUN or an ambiguous GET
// exhausts the budget and fails closed, never a fake success.
func TestVerifyLUNDeleted(t *testing.T) {
	t.Run("canonical NotFound is success", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) { writeVerifyLUNAbsent(w) })
		if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
			t.Fatalf("VerifyLUNDeleted: %v", err)
		}
	})

	t.Run("code-less HTTP 404 is success", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"not found"}`))
			})
		if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
			t.Fatalf("VerifyLUNDeleted: %v", err)
		}
	})

	t.Run("zero budgets fall back to package defaults", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 0, 0, nil,
			func(w http.ResponseWriter, req *http.Request) { writeVerifyLUNAbsent(w) })
		if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
			t.Fatalf("VerifyLUNDeleted with default budgets: %v", err)
		}
	})

	t.Run("present through budget fails closed", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				w.Write([]byte(verifyLUNPresentBody))
			})
		err := r.VerifyLUNDeleted(context.Background(), "lun-1")
		if err == nil {
			t.Fatal("VerifyLUNDeleted succeeded while the LUN is still present")
		}
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("code = %v, want DeadlineExceeded (err=%v)", status.Code(err), err)
		}
		if !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("error must state absence is not proven: %v", err)
		}
		if gets < 2 {
			t.Fatalf("verification GETs = %d, want bounded retries within the budget", gets)
		}
	})

	t.Run("delayed absence converges", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 2*time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				if gets < 3 {
					w.Write([]byte(verifyLUNPresentBody))
					return
				}
				writeVerifyLUNAbsent(w)
			})
		if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
			t.Fatalf("VerifyLUNDeleted should converge on delayed absence: %v", err)
		}
		if gets != 3 {
			t.Fatalf("verification GETs = %d, want 3", gets)
		}
	})

	t.Run("transient transport error then absence converges", func(t *testing.T) {
		failed := false
		gets := 0
		r := newVerifyTestReconciler(t, 2*time.Second, 10*time.Millisecond,
			func(req *http.Request) bool {
				if !failed && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1") {
					failed = true
					return true
				}
				return false
			},
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				if gets == 1 {
					w.Write([]byte(verifyLUNPresentBody))
					return
				}
				writeVerifyLUNAbsent(w)
			})
		if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
			t.Fatalf("VerifyLUNDeleted should retry an ambiguous GET and converge: %v", err)
		}
	})

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond,
			func(req *http.Request) bool {
				return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1")
			},
			func(w http.ResponseWriter, req *http.Request) {
				t.Error("transport-failed GET must not reach the handler")
			})
		err := r.VerifyLUNDeleted(context.Background(), "lun-1")
		if err == nil {
			t.Fatal("VerifyLUNDeleted treated a persistent transport error as absence")
		}
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("code = %v, want DeadlineExceeded (err=%v)", status.Code(err), err)
		}
		if !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("error must state absence is not proven: %v", err)
		}
	})

	t.Run("persistent 5xx fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"code":5000,"error":"backend unavailable"}`))
			})
		err := r.VerifyLUNDeleted(context.Background(), "lun-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("VerifyLUNDeleted = %v, want DeadlineExceeded on persistent 5xx", err)
		}
	})

	t.Run("403 is never absence", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"code":4030,"error":"forbidden"}`))
			})
		err := r.VerifyLUNDeleted(context.Background(), "lun-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("VerifyLUNDeleted = %v, want DeadlineExceeded on persistent 403", err)
		}
	})

	t.Run("parent context cancellation propagates", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(verifyLUNPresentBody))
			})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.VerifyLUNDeleted(ctx, "lun-1")
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled from the parent context (err=%v)", status.Code(err), err)
		}
	})
}

// TestVerifyLUNDeletedHTTPErrorNotAbsence pins the narrow safety fix on the
// REAL helper/SDK path (SDK client against a fake NGX API): an HTTP error
// status must never be converted into absence-success through the NotFound
// code path. Only an authoritative HTTP 404 carrying the canonical NotFound
// code (5011) or no code proves absence. A 404 with a different explicit
// code, any non-404 error status (even one carrying code 5011), or a
// transport failure stays ambiguous and fails closed.
func TestVerifyLUNDeletedHTTPErrorNotAbsence(t *testing.T) {
	notAbsence := []struct {
		name   string
		status int
		body   string
	}{
		{"404 with AlreadyExists code", 404, `{"code":710,"error":"exists"}`},
		{"404 with AttachedBusy code", 404, `{"code":534,"error":"busy"}`},
		{"404 with unknown explicit code", 404, `{"code":9999,"error":"other"}`},
		{"403 carrying NotFound code", 403, `{"code":5011,"error":"forbidden"}`},
		{"403 forbidden", 403, `{"code":5013,"error":"forbidden"}`},
		{"500 internal", 500, `{"code":5000,"error":"internal"}`},
		{"503 unavailable", 503, `{"error":"unavailable"}`},
	}
	for _, tc := range notAbsence {
		t.Run(tc.name, func(t *testing.T) {
			gets := 0
			r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
				func(w http.ResponseWriter, req *http.Request) {
					gets++
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				})
			err := r.VerifyLUNDeleted(context.Background(), "lun-1")
			if err == nil {
				t.Fatalf("HTTP %d body %s was converted to absence-success", tc.status, tc.body)
			}
			if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
				t.Fatalf("want fail-closed DeadlineExceeded 'absence NOT proven', got %v", err)
			}
			if gets < 1 {
				t.Fatalf("expected bounded verification GETs, got %d", gets)
			}
		})
	}
	t.Run("persistent transport failure is not absence", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond,
			func(req *http.Request) bool {
				return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1")
			},
			func(w http.ResponseWriter, req *http.Request) { writeVerifyLUNAbsent(w) })
		err := r.VerifyLUNDeleted(context.Background(), "lun-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("persistent transport must fail closed, got %v", err)
		}
	})
	t.Run("authoritative 404 controls still succeed", func(t *testing.T) {
		for _, body := range []string{`{"code":5011,"error":"not found"}`, `{"error":"not found"}`} {
			r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
				func(w http.ResponseWriter, req *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(body))
				})
			if err := r.VerifyLUNDeleted(context.Background(), "lun-1"); err != nil {
				t.Fatalf("authoritative absence %s: %v", body, err)
			}
		}
	})
}

// absenceCase is one HTTP status/body control for the bounded absence
// verifiers: wantAbsent marks the authoritative absence signals.
type absenceCase struct {
	name       string
	status     int
	body       string
	wantAbsent bool
}

// runAbsenceMatrix drives one verifier against the real SDK client and a
// fake NGX API returning a fixed status/body, pinning which responses prove
// absence and which must fail closed.
func runAbsenceMatrix(t *testing.T, cases []absenceCase, verify func(*Reconciler, context.Context) error) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gets := 0
			r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
				func(w http.ResponseWriter, req *http.Request) {
					gets++
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.body))
				})
			err := verify(r, context.Background())
			if tc.wantAbsent {
				if err != nil {
					t.Fatalf("authoritative absence %d %s: %v", tc.status, tc.body, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("HTTP %d body %s was converted to absence-success", tc.status, tc.body)
			}
			if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
				t.Fatalf("want fail-closed DeadlineExceeded 'absence NOT proven', got %v", err)
			}
			if gets < 1 {
				t.Fatalf("expected bounded verification GETs, got %d", gets)
			}
		})
	}
}

// TestVerifySnapshotDeleted pins the bounded read-after-delete semantics for
// snapshots on the REAL helper/SDK path: only an authoritative HTTP 404 with
// the canonical NotFound code (5011) or no code proves absence; a present
// snapshot or any ambiguous GET exhausts the budget and fails closed.
func TestVerifySnapshotDeleted(t *testing.T) {
	runAbsenceMatrix(t, []absenceCase{
		{"canonical NotFound is absence", 404, `{"code":5011,"error":"not found"}`, true},
		{"code-less HTTP 404 is absence", 404, `{"error":"not found"}`, true},
		{"404 with AlreadyExists code", 404, `{"code":710,"error":"exists"}`, false},
		{"404 with AttachedBusy code", 404, `{"code":534,"error":"busy"}`, false},
		{"404 with unknown explicit code", 404, `{"code":9999,"error":"other"}`, false},
		{"403 carrying NotFound code", 403, `{"code":5011,"error":"forbidden"}`, false},
		{"403 forbidden", 403, `{"code":5013,"error":"forbidden"}`, false},
		{"500 carrying NotFound code", 500, `{"code":5011,"error":"internal"}`, false},
		{"500 internal", 500, `{"code":5000,"error":"internal"}`, false},
		{"503 unavailable", 503, `{"error":"unavailable"}`, false},
	}, func(r *Reconciler, ctx context.Context) error { return r.VerifySnapshotDeleted(ctx, "snap-1") })

	t.Run("present through budget fails closed", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-1","ready":true}`))
			})
		err := r.VerifySnapshotDeleted(context.Background(), "snap-1")
		if err == nil {
			t.Fatal("VerifySnapshotDeleted succeeded while the snapshot is still present")
		}
		if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("want fail-closed DeadlineExceeded 'absence NOT proven', got %v", err)
		}
		if gets < 2 {
			t.Fatalf("verification GETs = %d, want bounded retries within the budget", gets)
		}
	})

	t.Run("delayed absence converges", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 2*time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				if gets < 3 {
					w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-1","ready":true}`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
			})
		if err := r.VerifySnapshotDeleted(context.Background(), "snap-1"); err != nil {
			t.Fatalf("VerifySnapshotDeleted should converge on delayed absence: %v", err)
		}
		if gets != 3 {
			t.Fatalf("verification GETs = %d, want 3", gets)
		}
	})

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond,
			func(req *http.Request) bool {
				return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/snapshot/snap-1")
			},
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
			})
		err := r.VerifySnapshotDeleted(context.Background(), "snap-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("persistent transport must fail closed, got %v", err)
		}
	})

	t.Run("parent context cancellation propagates", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(`{"id":"snap-1","name":"snap1","volume_id":"lun-1","ready":true}`))
			})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.VerifySnapshotDeleted(ctx, "snap-1")
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled from the parent context (err=%v)", status.Code(err), err)
		}
	})
}

// TestVerifyTargetDeleted pins the bounded read-after-delete semantics for
// iSCSI targets on the REAL helper/SDK path. Authoritative absence is HTTP
// 404 with the canonical NotFound code (5011), the target-deleted code (611),
// or no code. The IQN-scoped not-found code (521) is NOT target absence.
func TestVerifyTargetDeleted(t *testing.T) {
	runAbsenceMatrix(t, []absenceCase{
		{"canonical NotFound is absence", 404, `{"code":5011,"error":"not found"}`, true},
		{"TargetDeleted code with 404 is absence", 404, `{"code":611,"error":"target deleted"}`, true},
		{"code-less HTTP 404 is absence", 404, `{"error":"not found"}`, true},
		{"404 with IQNNotFound code is not target absence", 404, `{"code":521,"error":"iqn not found"}`, false},
		{"404 with AlreadyExists code", 404, `{"code":710,"error":"exists"}`, false},
		{"404 with unknown explicit code", 404, `{"code":9999,"error":"other"}`, false},
		{"403 carrying NotFound code", 403, `{"code":5011,"error":"forbidden"}`, false},
		{"403 carrying TargetDeleted code", 403, `{"code":611,"error":"forbidden"}`, false},
		{"500 carrying TargetDeleted code", 500, `{"code":611,"error":"internal"}`, false},
		{"500 internal", 500, `{"code":5000,"error":"internal"}`, false},
		{"503 unavailable", 503, `{"error":"unavailable"}`, false},
	}, func(r *Reconciler, ctx context.Context) error { return r.VerifyTargetDeleted(ctx, "target-1") })

	t.Run("present through budget fails closed", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				w.Write([]byte(`{"id":"target-1","name":"pvcvol1","luns":[]}`))
			})
		err := r.VerifyTargetDeleted(context.Background(), "target-1")
		if err == nil {
			t.Fatal("VerifyTargetDeleted succeeded while the target is still present")
		}
		if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("want fail-closed DeadlineExceeded 'absence NOT proven', got %v", err)
		}
		if gets < 2 {
			t.Fatalf("verification GETs = %d, want bounded retries within the budget", gets)
		}
	})

	t.Run("delayed absence converges", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 2*time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				if gets < 3 {
					w.Write([]byte(`{"id":"target-1","name":"pvcvol1","luns":[]}`))
					return
				}
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":611,"error":"target deleted"}`))
			})
		if err := r.VerifyTargetDeleted(context.Background(), "target-1"); err != nil {
			t.Fatalf("VerifyTargetDeleted should converge on delayed absence: %v", err)
		}
	})

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond,
			func(req *http.Request) bool {
				return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1")
			},
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
			})
		err := r.VerifyTargetDeleted(context.Background(), "target-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("persistent transport must fail closed, got %v", err)
		}
	})

	t.Run("parent context cancellation propagates", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(`{"id":"target-1","name":"pvcvol1","luns":[]}`))
			})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.VerifyTargetDeleted(ctx, "target-1")
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled from the parent context (err=%v)", status.Code(err), err)
		}
	})
}

// TestVerifyAuthGroupDeleted pins the bounded read-after-delete semantics for
// auth groups on the REAL helper/SDK path: only an authoritative HTTP 404
// with the canonical NotFound code (5011) or no code proves absence. The
// IQN-scoped not-found code (521) is NOT group absence.
func TestVerifyAuthGroupDeleted(t *testing.T) {
	runAbsenceMatrix(t, []absenceCase{
		{"canonical NotFound is absence", 404, `{"code":5011,"error":"not found"}`, true},
		{"code-less HTTP 404 is absence", 404, `{"error":"not found"}`, true},
		{"404 with IQNNotFound code is not group absence", 404, `{"code":521,"error":"iqn not found"}`, false},
		{"404 with AlreadyExists code", 404, `{"code":710,"error":"exists"}`, false},
		{"404 with unknown explicit code", 404, `{"code":9999,"error":"other"}`, false},
		{"403 carrying NotFound code", 403, `{"code":5011,"error":"forbidden"}`, false},
		{"500 carrying NotFound code", 500, `{"code":5011,"error":"internal"}`, false},
		{"500 internal", 500, `{"code":5000,"error":"internal"}`, false},
		{"503 unavailable", 503, `{"error":"unavailable"}`, false},
	}, func(r *Reconciler, ctx context.Context) error { return r.VerifyAuthGroupDeleted(ctx, "auth-1") })

	t.Run("present through budget fails closed", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				gets++
				w.Write([]byte(`{"id":"auth-1","name":"pvcvol1"}`))
			})
		err := r.VerifyAuthGroupDeleted(context.Background(), "auth-1")
		if err == nil {
			t.Fatal("VerifyAuthGroupDeleted succeeded while the auth group is still present")
		}
		if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("want fail-closed DeadlineExceeded 'absence NOT proven', got %v", err)
		}
		if gets < 2 {
			t.Fatalf("verification GETs = %d, want bounded retries within the budget", gets)
		}
	})

	t.Run("persistent transport error fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, 150*time.Millisecond, 20*time.Millisecond,
			func(req *http.Request) bool {
				return req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/v2/auth_group/auth-1")
			},
			func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":5011,"error":"not found"}`))
			})
		err := r.VerifyAuthGroupDeleted(context.Background(), "auth-1")
		if err == nil || status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "absence NOT proven") {
			t.Fatalf("persistent transport must fail closed, got %v", err)
		}
	})

	t.Run("parent context cancellation propagates", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				w.Write([]byte(`{"id":"auth-1","name":"pvcvol1"}`))
			})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := r.VerifyAuthGroupDeleted(ctx, "auth-1")
		if status.Code(err) != codes.Canceled {
			t.Fatalf("code = %v, want Canceled from the parent context (err=%v)", status.Code(err), err)
		}
	})
}

// TestWaitForISCSIUnmap pins the bounded unmap-convergence wait: the target
// delete may proceed only after the backend readback proves the LUN unmapped
// (target mapping list AND LUN protocol report). A silent no-op unmap or an
// ambiguous readback fails closed; authoritative object absence converges.
func TestWaitForISCSIUnmap(t *testing.T) {
	const mappedTargetBody = `{"id":"target-1","name":"pvcvol1","luns":[{"id":"lun-1","number":"0"}]}`
	const unmappedTargetBody = `{"id":"target-1","name":"pvcvol1","luns":[]}`
	const mappedLUNBody = `{"id":"lun-1","name":"pvcvol1","protocols":{"iscsi":"target-1"}}`
	const unmappedLUNBody = `{"id":"lun-1","name":"pvcvol1","protocols":{}}`

	t.Run("converged readback is success", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					w.Write([]byte(unmappedTargetBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					w.Write([]byte(unmappedLUNBody))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err != nil {
			t.Fatalf("WaitForISCSIUnmap on a converged readback: %v", err)
		}
	})

	t.Run("delayed convergence is success", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					gets++
					if gets < 2 {
						w.Write([]byte(mappedTargetBody))
					} else {
						w.Write([]byte(unmappedTargetBody))
					}
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					if gets < 2 {
						w.Write([]byte(mappedLUNBody))
					} else {
						w.Write([]byte(unmappedLUNBody))
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err != nil {
			t.Fatalf("WaitForISCSIUnmap should converge on a delayed unmap: %v", err)
		}
		if gets < 2 {
			t.Fatalf("convergence reads = %d, want a re-read after the first mapped observation", gets)
		}
	})

	t.Run("no-op unmap fails closed within the context budget", func(t *testing.T) {
		gets := 0
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					gets++
					w.Write([]byte(mappedTargetBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					w.Write([]byte(mappedLUNBody))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		err := r.WaitForISCSIUnmap(ctx, "target-1", "lun-1")
		if err == nil {
			t.Fatal("WaitForISCSIUnmap converged although the mapping never disappeared")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context deadline to bound the wait", err)
		}
		if gets < 1 {
			t.Fatalf("convergence reads = %d, want at least one readback", gets)
		}
	})

	t.Run("target-only mapping report still fails closed", func(t *testing.T) {
		// The target list says unmapped but the LUN still reports the iSCSI
		// protocol mapping: convergence requires BOTH signals.
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					w.Write([]byte(unmappedTargetBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					w.Write([]byte(mappedLUNBody))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if err := r.WaitForISCSIUnmap(ctx, "target-1", "lun-1"); err == nil {
			t.Fatal("WaitForISCSIUnmap converged while the LUN still reports an iSCSI mapping")
		}
	})

	t.Run("ambiguous target GET fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1") {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"code":5000,"error":"internal"}`))
					return
				}
				w.Write([]byte(unmappedLUNBody))
			})
		err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1")
		if err == nil {
			t.Fatal("WaitForISCSIUnmap treated an ambiguous target GET as convergence")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ambiguous GET must fail closed immediately, not wait: %v", err)
		}
	})

	t.Run("404 with other code is not target absence", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1") {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":710,"error":"exists"}`))
					return
				}
				w.Write([]byte(unmappedLUNBody))
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err == nil {
			t.Fatal("WaitForISCSIUnmap treated a contradictory 404 as target absence")
		}
	})

	t.Run("ambiguous LUN GET fails closed", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					w.Write([]byte(unmappedTargetBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(`{"code":5013,"error":"forbidden"}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err == nil {
			t.Fatal("WaitForISCSIUnmap treated an ambiguous LUN GET as convergence")
		}
	})

	t.Run("authoritative target absence is converged", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1") {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
					return
				}
				w.Write([]byte(mappedLUNBody))
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err != nil {
			t.Fatalf("authoritative target absence must converge: %v", err)
		}
	})

	t.Run("authoritative LUN absence is converged", func(t *testing.T) {
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/target/iscsi/target-1"):
					w.Write([]byte(mappedTargetBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/lun/lun-1"):
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte(`{"code":5011,"error":"not found"}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		if err := r.WaitForISCSIUnmap(context.Background(), "target-1", "lun-1"); err != nil {
			t.Fatalf("authoritative LUN absence must converge: %v", err)
		}
	})
}

// TestSelectPortalGroupConfiguredAbsentFailsClosed pins the configured
// portal-group contract: a configured name that was renamed or removed after
// startup fails closed with NotFound; an unconfigured driver keeps selecting
// the first available group.
func TestSelectPortalGroupConfiguredAbsentFailsClosed(t *testing.T) {
	const configuredList = `[{"id":"pg1","name":"pg-configured","owner":"o"}]`
	const driftedList = `[{"id":"pg-other","name":"unrelated-group","owner":"o"},{"id":"pg1","name":"pg-renamed","owner":"o"}]`
	const detailBody = `{"id":"pg1","name":"pg-configured","owner":"o","listen_ips":[{"id":"lip1","interface_name":"igb0","ip_address":"192.168.1.201","owner":"o"}]}`

	newPortalReconciler := func(t *testing.T, configured, list string) *Reconciler {
		t.Helper()
		r := newVerifyTestReconciler(t, time.Second, 10*time.Millisecond, nil,
			func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/api/v2/portal_group"):
					w.Write([]byte(list))
				case strings.HasSuffix(req.URL.Path, "/api/v2/portal_group/pg1"):
					w.Write([]byte(detailBody))
				case strings.HasSuffix(req.URL.Path, "/api/v2/portal_group/pg-other"):
					w.Write([]byte(`{"id":"pg-other","name":"unrelated-group","owner":"o","listen_ips":[{"id":"lip9","interface_name":"igb9","ip_address":"10.99.99.1","owner":"o"}]}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
		r.PortalGroup = configured
		return r
	}

	t.Run("configured group present is selected", func(t *testing.T) {
		r := newPortalReconciler(t, "pg-configured", configuredList)
		id, ips, err := r.SelectPortalGroup(context.Background())
		if err != nil || id != "pg1" || len(ips) != 1 || ips[0] != "192.168.1.201" {
			t.Fatalf("SelectPortalGroup = %q %v %v, want pg1 with its portal IP", id, ips, err)
		}
	})

	t.Run("configured group renamed fails closed", func(t *testing.T) {
		r := newPortalReconciler(t, "pg-configured", driftedList)
		_, _, err := r.SelectPortalGroup(context.Background())
		if status.Code(err) != codes.NotFound {
			t.Fatalf("code = %v, want NotFound for the renamed configured group (err=%v)", status.Code(err), err)
		}
	})

	t.Run("configured group removed fails closed", func(t *testing.T) {
		r := newPortalReconciler(t, "pg-configured", `[{"id":"pg-other","name":"unrelated-group","owner":"o"}]`)
		_, _, err := r.SelectPortalGroup(context.Background())
		if status.Code(err) != codes.NotFound {
			t.Fatalf("code = %v, want NotFound for the removed configured group (err=%v)", status.Code(err), err)
		}
	})

	t.Run("no portal groups fails closed", func(t *testing.T) {
		r := newPortalReconciler(t, "pg-configured", `[]`)
		_, _, err := r.SelectPortalGroup(context.Background())
		if status.Code(err) != codes.NotFound {
			t.Fatalf("code = %v, want NotFound when no portal groups exist (err=%v)", status.Code(err), err)
		}
	})

	t.Run("unconfigured driver selects the first group", func(t *testing.T) {
		r := newPortalReconciler(t, "", driftedList)
		id, _, err := r.SelectPortalGroup(context.Background())
		if err != nil || id != "pg-other" {
			t.Fatalf("SelectPortalGroup = %q %v, want the first group pg-other when unconfigured", id, err)
		}
	})
}
