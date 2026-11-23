package logutils

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

func startMaintenance(dir string, retention int, interval time.Duration, state *maintenance) func() {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	var once sync.Once
	var worker sync.WaitGroup
	pass := func() {
		if err := rotate(dir, state.filePrefix, state.active); err != nil {
			state.logger.Error("log rotation failed", zap.Error(err), zap.String("dir", dir))
		}
		if err := cleanOld(dir, state.filePrefix, retention); err != nil {
			state.logger.Error("log retention cleanup failed", zap.Error(err), zap.String("dir", dir))
		}
	}
	worker.Add(1)
	go func() {
		defer worker.Done()
		pass()
		for {
			select {
			case <-ticker.C:
				pass()
			case <-done:
				return
			}
		}
	}()
	return func() {
		once.Do(func() {
			ticker.Stop()
			close(done)
			worker.Wait()
		})
	}
}

func rotate(dir, prefix, active string) error {
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(dir, logPattern(prefix, ".log")))
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-DefaultRotationAge)
	for _, path := range matches {
		if filepath.Clean(path) == active {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := compress(path); err != nil {
			continue
		}
	}
	return nil
}

func compress(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	dst, err := os.Create(path + ".gz")
	if err != nil {
		_ = src.Close()
		return err
	}
	gz := gzip.NewWriter(dst)
	_, copyErr := io.Copy(gz, src)
	closeErr := gz.Close()
	dstErr := dst.Close()
	_ = src.Close()
	if copyErr != nil || closeErr != nil || dstErr != nil {
		_ = os.Remove(path + ".gz")
		return firstErr(copyErr, closeErr, dstErr)
	}
	return os.Remove(path)
}

func cleanOld(dir, prefix string, retention int) error {
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(dir, logPattern(prefix, ".log.gz")))
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(retention) * 24 * time.Hour)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
