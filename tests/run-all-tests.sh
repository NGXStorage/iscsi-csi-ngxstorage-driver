#!/usr/bin/env bash
set -euo pipefail

driver_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
driver_name="$(basename "$driver_root")"
protocol="${driver_name,,}"
scenario_runner="$driver_root/tests/scenario/run.sh"
cache_root="$HOME/.cache/ngxstorage"
case "$cache_root" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "refusing temporary cache under /tmp or /var/tmp" >&2; exit 2 ;; esac
# Shared verified-prebuilt gate (same implementation the sanity cloud and
# scenario consumers use); defines csi_prebuilt_enabled/csi_prebuilt_verify.
# shellcheck source=tests/sanity/cloud/prebuilt.sh
source "$driver_root/tests/sanity/cloud/prebuilt.sh"
suite="${1:-local}"
shift || true
allow_live=0
include_disruptive=0
include_stress=0
scenario_name=""
sanity_mode=""

usage() {
  cat <<EOF
Usage: $0 [all|local|sanity|e2e|external-e2e|scenarios|scenario NAME|list] [options]

  local          unit + entrypoint contract + E2E compile/skip + race + vet
  sanity MODE    local live backend with test-process coordination or cloud csi-sanity
  e2e            client-go E2E against an already-deployed driver
  external-e2e   Kubernetes External.Storage E2E against an already-deployed driver
  scenario NAME  one live scenario, dispatched by this driver's tests/scenario/run.sh
  scenarios      all ordinary scenarios; excludes host reboot and stress by default
  all            local plus all live suites above (requires --allow-live)
  list           show suites and safety notes

Options:
  --allow-live          confirm that live suites may create/delete test resources
  --include-disruptive  include volume-recovery-host-reboot in scenarios
  --include-stress      include benchmark-stress-* scenarios

Environment:
  Local sanity requires one explicit runtime profile: STORAGE_API_KEY,
  STORAGE_IPS, STORAGE_POOL_NAME and STORAGE_FC_TARGET_NAMES, from a complete
  tests/sanity/local/config.env (file mode, authoritative) or the exported
  environment (env mode). Missing/partial profiles fail fast; no values are
  ever scraped from documentation. See tests/sanity/local/config.env.example.
  Scenarios require explicit NGX_CONTROLLER, STORAGE_API_KEY (or NGX_API_KEY)
  and NGX_POOL_NAME; see tests/README.md.
  CSI_PREBUILT_DIR       optional directory of verified prebuilt test binaries
                         (csi-sanity, sanity-proxy, csi-race, e2e.test). Passed
                         through unchanged to the sanity cloud / scenario
                         consumers, which verify them against the manifest.
  CSI_PREBUILT_MANIFEST  JSON manifest with per-binary sha256, goos, goarch,
                         source_fingerprint, go_version, build_flags, built_at.
                         When \$CSI_PREBUILT_DIR is set, the e2e suite execs
                         e2e.test only after the shared fail-closed gate
                         (tests/sanity/cloud/prebuilt.py): sha256, host
                         goos/goarch, CURRENT-source fingerprint equality and
                         a smoke probe. A missing binary/manifest or any failed
                         check is a hard failure; the regular go test build
                         path is used only when CSI_PREBUILT_DIR is unset.

NFS multi-version-k8s is intentionally not automated here: its current script
resets kubeadm and removes host Kubernetes state. Run only after that harness is
refactored and separately reviewed.
EOF
}

fail() { printf '[%s tests] ERROR: %s\n' "$protocol" "$*" >&2; exit 2; }

while (($#)); do
  case "$1" in
    --allow-live) allow_live=1 ;;
    --include-disruptive) include_disruptive=1 ;;
    --include-stress) include_stress=1 ;;
    -h|--help) usage; exit 0 ;;
    *)
      if [[ "$suite" == "sanity" && -z "$sanity_mode" && ( "$1" == "local" || "$1" == "cloud" ) ]]; then
        sanity_mode="$1"
      elif [[ "$suite" == "scenario" && -z "$scenario_name" ]]; then
        scenario_name="$1"
      else
        fail "unknown argument: $1"
      fi
      ;;
  esac
  shift
done

if [[ "$suite" == -h || "$suite" == --help ]]; then usage; exit 0; fi
if [[ "$suite" == "list" ]]; then
  usage
  printf '\nDriver: %s (%s)\n' "$driver_name" "$protocol"
  printf 'Scenario cases: '
  find "$driver_root/tests/scenario" -mindepth 2 -maxdepth 2 -type f -name run-test.sh -printf '%h\n' 2>/dev/null \
    | sed "s|$driver_root/tests/scenario/||" | sort | paste -sd ' ' -
  exit 0
fi

case "$suite" in
  all|local|sanity|e2e|external-e2e|scenarios|scenario) ;;
  *) usage >&2; fail "unsupported suite: $suite" ;;
esac

needs_live=0
case "$suite" in all|sanity|e2e|external-e2e|scenarios|scenario) needs_live=1 ;; esac
if ((needs_live && !allow_live)); then
  fail "'$suite' can mutate backend/cluster resources; rerun with --allow-live after checking the target"
fi

# Load only the selected sanity method's local, Git-ignored configuration.
config_mode=""
if [[ "$suite" == sanity ]]; then config_mode="${sanity_mode:-local}"; fi
if [[ "$suite" == scenario && "$scenario_name" == sanity ]]; then config_mode="cloud"; fi
if [[ -n "$config_mode" && -f "$driver_root/tests/sanity/$config_mode/config.env" ]]; then
  set -a
  source "$driver_root/tests/sanity/$config_mode/config.env"
  set +a
fi

# B12: the former fallback that scraped unset STORAGE_* values from the
# internal cluster-infrastructure lab markdown (Integrations Tests tree) is
# REMOVED. Local sanity now uses the single open runtime profile contract
# (tests/sanity/local/runtime-profile.sh, checked in run_sanity below):
# missing/partial profiles fail fast, and no documentation is ever read for
# runtime values.

mkdir -p "$cache_root"
export TMPDIR="$cache_root"
export GOCACHE="${GOCACHE:-$cache_root/ngxstorage-csi-${protocol}-go-cache}"

# Verified-prebuilt dispatch inputs (optional). Passed through UNCHANGED to
# the sanity cloud / scenario consumers (tests/sanity/cloud/helpers.sh and
# tests/scenario/lib.sh csi_race_prepare), which verify them against the
# manifest themselves. The dispatcher verifies the e2e.test binary it execs
# directly through the SAME shared gate (csi_prebuilt_verify); it never
# re-validates the other binaries.
[[ -z "${CSI_PREBUILT_DIR:-}" ]] || export CSI_PREBUILT_DIR
[[ -z "${CSI_PREBUILT_MANIFEST:-}" ]] || export CSI_PREBUILT_MANIFEST

preflight_sanity() {
  [[ -n "${STORAGE_API_KEY:-}" ]] || fail "sanity requires STORAGE_API_KEY"
  [[ -x "$driver_root/cmd/csi-driver/${protocol}-csi-ngxstorage-driver" ]] \
    || fail "sanity driver binary is missing; build it first with the driver's build script"
  [[ -x "$CSI_SANITY_BIN" ]] || fail "shared csi-sanity binary is missing; resolved by run_sanity"
}

preflight_e2e() {
  [[ -n "${KUBECONFIG:-}" && -f "$KUBECONFIG" ]] || fail "e2e requires an existing KUBECONFIG"
}

preflight_external() {
  [[ -n "${KUBECONFIG:-}" && -f "$KUBECONFIG" ]] || fail "external-e2e requires an existing KUBECONFIG"
  [[ -n "${E2E_TEST_BINARY:-}" && -x "$E2E_TEST_BINARY" ]] || fail "E2E_TEST_BINARY must name an executable Kubernetes e2e.test binary"
  command -v kubectl >/dev/null || fail "kubectl is required"
}

preflight_scenario() {
  if [[ "$suite" == sanity && "${sanity_mode:-local}" == cloud ]] || [[ "$suite" == scenario && "$scenario_name" == sanity ]]; then
    if [[ -n "${KUBECONFIG:-}" && ! -f "$KUBECONFIG" ]]; then fail "KUBECONFIG file not found"; fi
    if [[ -n "${KUBE_CONTEXT:-}" ]]; then
      local kube_command="${KUBE_BIN:-}"
      if [[ -z "$kube_command" ]]; then [[ "${BENCH:-}" == ocp2 ]] && kube_command=oc || kube_command=kubectl; fi
      command -v "$kube_command" >/dev/null || fail "$kube_command is required"
      local current_context
      current_context="$("$kube_command" config current-context)" || fail "cannot read Kubernetes current context"
      [[ "$current_context" == "$KUBE_CONTEXT" ]] || fail "current Kubernetes context does not match KUBE_CONTEXT"
    fi
  fi
  [[ -x "$scenario_runner" ]] || [[ -f "$scenario_runner" ]] || fail "driver scenario runner not found: $scenario_runner"
  [[ "${BENCH:-}" == "ocp2" || "${BENCH:-}" == "k8s" ]] \
    || fail "scenario execution requires explicit BENCH=ocp2 or BENCH=k8s"
  # Explicit runtime profile only — name every missing variable; no defaults
  # are filled in from documentation or lab data. The scenario runner accepts
  # STORAGE_API_KEY or NGX_API_KEY as the credential (same precedence).
  local missing=""
  [[ -n "${NGX_CONTROLLER:-}" ]] || missing="$missing NGX_CONTROLLER"
  [[ -n "${STORAGE_API_KEY:-}" || -n "${NGX_API_KEY:-}" ]] || missing="$missing STORAGE_API_KEY/NGX_API_KEY"
  [[ -n "${NGX_POOL_NAME:-}" ]] || missing="$missing NGX_POOL_NAME"
  [[ -z "$missing" ]] \
    || fail "scenario runtime profile incomplete; missing:$missing — export explicit values from an approved runtime profile (see tests/README.md and tests/sanity/cloud/config.env.example)"
  local sdk_cli="${NGX_SDK_CLI:-$driver_root/tests/tools/bin/ngxstorage-cli}"
  [[ -x "$sdk_cli" ]] || fail "scenario requires the driver-local ngxstorage-cli or an executable NGX_SDK_CLI override"
}

run_local() {
  printf '\n== %s local test gates ==\n' "$driver_name"
  RUN_E2E_COMPILE=1 RUN_RACE=1 RUN_VET=1 bash "$driver_root/tests/run-unit-tests.sh"
}

run_sanity() {
  # Resolve the SHARED csi-sanity binary once (verified prebuilt, or build from
  # the pinned fork commit). Via the sudo full-mock wrapper CSI_SANITY_BIN is
  # already a pinned resolved path; invoked directly, resolve here. Never a
  # stale driver-local cmd/csi-sanity copy.
  # Runtime profile FIRST: a missing/partial profile must fail before any
  # tool (git/go/etc.) is invoked while resolving the shared sanity binary.
  if [[ "${sanity_mode:-local}" != cloud ]]; then
    # Local sanity connection settings come from one open runtime profile
    # (config.env and/or environment); a partial profile fails here, before
    # any live access. There is no fallback channel that scrapes
    # documentation for lab defaults.
    # shellcheck source=tests/sanity/local/runtime-profile.sh
    source "$driver_root/tests/sanity/local/runtime-profile.sh"
    sanity_profile_preflight "$driver_root/tests/sanity/local/config.env" \
      || fail "sanity runtime profile incomplete; see tests/sanity/local/config.env.example"
  fi
  export CSI_SANITY_BIN="${CSI_SANITY_BIN:-$driver_root/build/shared-sanity/csi-sanity}"
  # A missing shared binary is a fail-closed precondition: report it WITHOUT
  # invoking any tool (git/go/curl). The shared binary is produced by the
  # build script before this dispatcher runs; resolution never builds on
  # demand here, which keeps the runtime-profile negative contract intact.
  [[ -x "$CSI_SANITY_BIN" ]] \
    || fail "shared csi-sanity binary is missing; build it with tests/sanity/build-shared-sanity.sh (fork @${CSI_TEST_FORK_SHA})"
  if [[ "${sanity_mode:-local}" == cloud ]]; then
    preflight_sanity
    printf '\n== %s deployed csi-sanity ==\n' "$driver_name"
    bash "$driver_root/tests/scenario/sanity/run-test.sh"
  else
    preflight_sanity
    printf '\n== %s csi-sanity (live backend, process-local mock coordination) ==\n' "$driver_name"
    bash "$driver_root/tests/sanity/local/mock-coordination.sh"
  fi
}

# Explicit prebuilt mode (CSI_PREBUILT_DIR set) is fail-closed: the shared
# gate (prebuilt.py via csi_prebuilt_verify) checks the manifest entry,
# sha256, host goos/goarch, CURRENT-source fingerprint equality and the smoke
# probe BEFORE exec, and the verified binary is what executes. A missing
# binary/manifest or any failed check is a hard failure — never a fallback to
# a host go build. The go test build path runs only when prebuilt mode is NOT
# selected (CSI_PREBUILT_DIR unset).
run_e2e() {
  preflight_e2e
  if csi_prebuilt_enabled; then
    local verified
    verified="$(csi_prebuilt_verify e2e.test)" \
      || fail "prebuilt e2e.test rejected by shared verification (fail-closed); see the error above"
    printf '\n== %s client-go E2E (verified prebuilt e2e.test, live cluster) ==\n' "$driver_name"
    (cd "$driver_root/tests/e2e" && E2E_RUN=1 "$verified" -test.timeout=45m -test.count=1 -test.v)
  else
    printf '\n== %s client-go E2E (live cluster) ==\n' "$driver_name"
    (cd "$driver_root/tests/e2e" && E2E_RUN=1 go test -timeout=45m -count=1 -v ./...)
  fi
}

run_external() {
  preflight_external
  printf '\n== %s External.Storage E2E (live cluster) ==\n' "$driver_name"
  bash "$driver_root/tests/external-e2e/run.sh"
}

run_scenario() {
  local name="$1"
  [[ -f "$driver_root/tests/scenario/$name/run-test.sh" ]] || fail "scenario not found: $name"
  preflight_scenario
  printf '\n== %s scenario: %s (%s) ==\n' "$driver_name" "$name" "$BENCH"
  ALLOW_LIVE=1 bash "$scenario_runner" "$name"
}

run_scenarios() {
  local dir name status=0
  preflight_scenario
  while IFS= read -r dir; do
    name="${dir##*/}"
    case "$name" in
      volume-recovery-host-reboot)
        if ((!include_disruptive)); then
          printf '[skip] %s: host reboot requires --include-disruptive\n' "$name"
          continue
        fi
        ;;
      benchmark-stress-*)
        if ((!include_stress)); then
          printf '[skip] %s: long-running stress requires --include-stress\n' "$name"
          continue
        fi
        ;;
    esac
    run_scenario "$name" || status=1
  done < <(find "$driver_root/tests/scenario" -mindepth 2 -maxdepth 2 -type f -name run-test.sh -printf '%h\n' | sort)
  return "$status"
}

case "$suite" in
  local) run_local ;;
  sanity) if [[ "${sanity_mode:-local}" == cloud ]]; then run_scenario sanity; else run_sanity; fi ;;
  e2e) run_e2e ;;
  external-e2e) run_external ;;
  scenario) [[ -n "$scenario_name" ]] || fail "scenario name is required"; run_scenario "$scenario_name" ;;
  scenarios) run_scenarios ;;
  all)
    preflight_sanity
    preflight_e2e
    preflight_external
    preflight_scenario
    run_local
    run_sanity
    run_e2e
    run_external
    run_scenarios
    ;;
esac
