// Package logutils provides the common Zap logger used by NGXStorage CSI drivers.
package logutils

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	DefaultRetentionDays = 30
	DefaultRotationAge   = 24 * time.Hour
	LogDirectoryEnv      = "NGXSTORAGE_CSI_LOG_DIR"
)

var levels = map[string]zapcore.Level{
	"debug": zapcore.DebugLevel, "info": zapcore.InfoLevel, "warn": zapcore.WarnLevel,
	"error": zapcore.ErrorLevel, "dpanic": zapcore.DPanicLevel, "panic": zapcore.PanicLevel,
	"fatal": zapcore.FatalLevel,
}

var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Config describes one process logger. Directory is normally supplied from
// NGXSTORAGE_CSI_LOG_DIR by the node deployment; an empty directory is stdout-only.
type Config struct {
	Directory     string
	FilePrefix    string
	NodeID        string
	ConsoleLevel  string
	RetentionDays int
	Interval      time.Duration
}

// Instance owns the Zap logger and its maintenance goroutine.
type Instance struct {
	Logger      *zap.Logger
	stop        func()
	file        *os.File
	active      string
	filePrefix  string
	maintenance *maintenance
	closeOnce   sync.Once
	closeErr    error
}

type maintenance struct {
	logger     *zap.Logger
	active     string
	filePrefix string
}

func ParseLevel(value string) (zapcore.Level, bool) {
	level, ok := levels[strings.ToLower(strings.TrimSpace(value))]
	return level, ok
}

// NewFromEnvironment constructs a CSI process logger using the standard node
// deployment environment. driverNamePrefix isolates parallel chart installs.
func NewFromEnvironment(consoleLevel, nodeID, protocol, driverNamePrefix string) (*Instance, error) {
	return New(Config{
		Directory:    os.Getenv(LogDirectoryEnv),
		FilePrefix:   ScopedPrefix(protocol, driverNamePrefix),
		NodeID:       nodeID,
		ConsoleLevel: consoleLevel,
	})
}

// ScopedPrefix creates a stable, filesystem-safe prefix for one driver
// instance. Helm passes the same driverNamePrefix to controller and node.
func ScopedPrefix(protocol, driverNamePrefix string) string {
	protocol = strings.ToLower(strings.Trim(strings.TrimSpace(protocol), ".-"))
	prefix := strings.ToLower(strings.Trim(strings.Trim(strings.TrimSpace(driverNamePrefix), `"`), ".-"))
	if prefix == "" {
		return protocol
	}
	return protocol + "-" + prefix
}

// New constructs a logger. Invalid configuration is returned before any file
// is opened, so callers can choose an explicit console-only fallback.
func New(config Config) (*Instance, error) {
	config.FilePrefix = strings.TrimSpace(config.FilePrefix)
	if !safeName.MatchString(config.FilePrefix) {
		return nil, fmt.Errorf("invalid log file prefix %q", config.FilePrefix)
	}
	level, ok := ParseLevel(config.ConsoleLevel)
	if !ok {
		level = zapcore.InfoLevel
	}
	instance := &Instance{filePrefix: config.FilePrefix}
	console := newConsoleCore(level)
	node := config.NodeID
	if node == "" || node == "defaultNodeIDValue" {
		node = "controller"
	}
	if !safeName.MatchString(strings.ToLower(node)) {
		return nil, fmt.Errorf("invalid log node id %q", node)
	}
	if strings.TrimSpace(config.Directory) == "" {
		instance.Logger = zap.New(console, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
		return instance, nil
	}
	dir := filepath.Clean(strings.TrimSpace(config.Directory))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory %q: %w", dir, err)
	}
	filename := filepath.Join(dir, config.FilePrefix+"-"+node+"-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".log")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", filename, err)
	}
	instance.file, instance.active = file, filepath.Clean(filename)
	fileCore := zapcore.NewCore(zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey: "msg", LevelKey: "level", TimeKey: "time", CallerKey: "caller",
		EncodeLevel: zapcore.CapitalLevelEncoder, EncodeTime: zapcore.RFC3339TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder, EncodeCaller: zapcore.FullCallerEncoder,
	}), zapcore.AddSync(file), zap.NewAtomicLevelAt(zapcore.DebugLevel))
	instance.Logger = zap.New(zapcore.NewTee(console, fileCore), zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
	interval := config.Interval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	retention := config.RetentionDays
	if retention <= 0 {
		retention = DefaultRetentionDays
	}
	maintenanceState := &maintenance{logger: instance.Logger, active: instance.active, filePrefix: config.FilePrefix}
	instance.maintenance = maintenanceState
	instance.stop = startMaintenance(dir, retention, interval, maintenanceState)
	return instance, nil
}

func (i *Instance) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		if i.stop != nil {
			i.stop()
		}
		if i.Logger != nil {
			_ = i.Logger.Sync()
		}
		if i.file != nil {
			i.closeErr = i.file.Close()
		}
	})
	return i.closeErr
}

func newConsoleCore(level zapcore.Level) zapcore.Core {
	return zapcore.NewCore(zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
		MessageKey: "msg", LevelKey: "level", TimeKey: "time", CallerKey: "caller",
		EncodeLevel: zapcore.CapitalColorLevelEncoder, EncodeTime: zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder, EncodeCaller: zapcore.ShortCallerEncoder,
		ConsoleSeparator: "\t",
	}), zapcore.AddSync(os.Stdout), zap.NewAtomicLevelAt(level))
}

func logPattern(prefix, suffix string) string { return prefix + "-*" + suffix }

// SDKLogger adapts Zap to the SDK's printf-style logger without exposing secrets.
type SDKLogger struct{ logger *zap.Logger }

func NewSDKLogger(logger *zap.Logger) SDKLogger       { return SDKLogger{logger: logger} }
func (l SDKLogger) Debugf(format string, args ...any) { l.logger.Sugar().Debugf(format, args...) }
func (l SDKLogger) Infof(format string, args ...any)  { l.logger.Sugar().Infof(format, args...) }
func (l SDKLogger) Warnf(format string, args ...any)  { l.logger.Sugar().Warnf(format, args...) }
func (l SDKLogger) Errorf(format string, args ...any) { l.logger.Sugar().Errorf(format, args...) }
