package driver

import (
	"context"
	"errors"
	"fmt"
	helper "github.com/ngxstorage/iscsi-csi-ngxstorage-driver/pkg/helper"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	blockmount "github.com/NGXStorage/csi-ngxstorage-utils/blockmount"
	coord "github.com/NGXStorage/csi-ngxstorage-utils/coord"
	kubeutils "github.com/NGXStorage/csi-ngxstorage-utils/kubernetes"
	nodestate "github.com/NGXStorage/csi-ngxstorage-utils/node"
	logutils "github.com/ngxstorage/go-logutils"
	ngxstorage "github.com/ngxstorage/ngxstorage-sdk-go/pkg/ngxstorage"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
)

const (
	DriverName    = "iscsi.csi.ngxstorage.com"
	DriverVersion = "26.01"
)

var (
	// iSCSI block storage supports staging (device attach + format + mount to
	// staging path) and node-side expansion (filesystem resize after LUN
	// expand).
	supportedNodeCapabilities = []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	}
	supportedControllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_GET_VOLUME,
		// PUBLISH_READONLY is intentionally not advertised: the controller
		// maps a LUN to a shared iSCSI target with no per-node read-only state
		// (a read-only flip cannot be told apart from an idempotent repeat),
		// so read-only is enforced by the node plugin's readonly bind mounts.
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		csi.ControllerServiceCapability_RPC_GET_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	}
	// NGX iSCSI block-storage access modes. ReadOnlyMany PVCs are represented
	// by CSI as MULTI_NODE_READER_ONLY, including when multiple readers happen
	// to run on the same Kubernetes node.
	supportedAccessModes = []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	}
)

// Driver represents a Container Storage Interface (CSI) driver implementation.
type Driver struct {
	logger *zap.Logger
	NodeId string
	Role   string

	Endpoint string

	Server *grpc.Server

	// client is the unified NGX API v2 SDK client. Driver-specific CSI logic
	// (volume-id cache, iSCSI device handling) lives in the driver, not the
	// SDK.
	client *ngxstorage.Client

	// portalGroup is the optional pre-configured iSCSI portal group name.
	// When empty, the driver selects the first available portal group.
	portalGroup string

	// poolOwner is the controller owning the configured pool, resolved at
	// startup. Targets and portal groups must live on the same controller as
	// the pool.
	poolOwner string

	Name    string
	Version string
	Ready   bool

	mountPermissions uint64
	mounter          mount.Interface
	bindMount        func(source, target, fstype string, options []string) error
	pathStat         func(string) (os.FileInfo, error)
	// nodeChmod is a test seam for filesystem-level chmod behavior.
	nodeChmod       func(string, os.FileMode) error
	sessionStateDir string
	// coordination serializes cross-node/controller operations that mutate the
	// same shared backend or filesystem state. It is injected in tests and
	// constructed from in-cluster configuration before the CSI socket is served.
	coordination coord.Coordinator
	// filesystemOps is a test seam for the split probe/format/verify/mount
	// workflow. Production uses hostFilesystemOperations.
	filesystemOps     helper.FilesystemOperations
	filesystemResizer helper.FilesystemResizer
	// attachmentLookup queries the Kubernetes VolumeAttachment API for the
	// remaining publishers of a shared per-volume iSCSI target.
	attachmentLookup kubeutils.AttachmentLookup
	AccessModes      []csi.VolumeCapability_AccessMode_Mode
	csCap            []*csi.ControllerServiceCapability
	nsCap            []*csi.NodeServiceCapability
	nodeState        nodestate.State

	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer
}

// InputParameters represents configuration parameters required to initialize
// the iSCSI CSI driver.
type InputParameters struct {
	Logger       *zap.Logger
	Endpoint     string
	ApiKey       string
	Ip           string
	PoolName     string
	PortalGroup  string // optional pre-configured iSCSI portal group name
	NodeId       string
	Role         string
	Log          string
	DriverPrefix string
}

// filesystemStager assembles the filesystem orchestration service from the
// current dependency seams (tests inject Ops/Resizer/Coord before the RPC).
func (d *Driver) filesystemStager() *helper.FilesystemStager {
	return &helper.FilesystemStager{Ops: d.filesystemOps, Resizer: d.filesystemResizer, Coord: d.coordination, Log: d.logger}
}

// reconciler assembles the backend reconcile service for controller RPCs.
func (d *Driver) reconciler() *helper.Reconciler {
	return &helper.Reconciler{Client: d.client, PoolOwner: d.poolOwner, PortalGroup: d.portalGroup, VolumeIDPrefix: volumeIDPrefix, Log: d.logger}
}

// initiatorRegistrar assembles the node-side IQN registration service.
func (d *Driver) initiatorRegistrar() *helper.InitiatorRegistrar {
	return &helper.InitiatorRegistrar{Coord: d.coordination, Recon: d.reconciler(), Log: d.logger}
}

func (d *Driver) log() *zap.Logger {
	if d != nil && d.logger != nil {
		return d.logger
	}
	return zap.NewNop()
}

// ResolveDriverName returns the effective CSI driver name for the configured
// prefix. An empty prefix preserves the canonical iscsi.csi.ngxstorage.com name.
func ResolveDriverName(prefix string) string {
	prefix = strings.TrimSpace(strings.ToLower(prefix))
	prefix = strings.Trim(prefix, "\"")
	prefix = strings.TrimSuffix(prefix, ".")
	if prefix == "" {
		return DriverName
	}
	return prefix + "." + DriverName
}

// String returns a redacted diagnostic representation of driver parameters.
// The API key is never logged.
func (p *InputParameters) String() string {
	return fmt.Sprintf("[ %s %s %s %s [REDACTED] %s %s %s %s ]",
		p.Endpoint, p.Ip, p.PoolName, p.PortalGroup, p.NodeId, p.Role, p.Log, p.DriverPrefix)
}

/*
 * NewDriver validates local configuration, creates the NGX client, and
 * resolves a serviceable controller/pool placement before any CSI socket
 * is served. Initialization is bounded to 60 seconds and never logs the
 * API key.
 *
 * @param parameters: Requires controller address, canonical pool name, API
 *                    key, unix endpoint, optional portal group, and a node
 *                    ID for the node role.
 *
 * @return *Driver: Initialized but not-yet-serving controller or node service.
 * @return error: InvalidArgument for local configuration, NotFound for an
 *         absent configured pool, Unavailable for unusable controller mode.
 */
func NewDriver(parameters InputParameters) (*Driver, error) {
	return newDriver(parameters, nil)
}

// NewDriverWithHTTPClient provides isolated transport injection for tests.
func NewDriverWithHTTPClient(parameters InputParameters, client *http.Client) (*Driver, error) {
	return newDriver(parameters, client)
}

func newDriver(parameters InputParameters, injectedClient *http.Client) (*Driver, error) {
	logger := parameters.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	logger.Info("NewDriver(): init")
	logger.Debug("NewDriver(): parameters",
		zap.String("parameters", parameters.String()),
	)

	// Validate required configuration fields.
	if parameters.Ip == "" || parameters.Ip == "defaultIPValue" {
		logger.Error("NewDriver(): IP is empty, must be provided")
		return nil, status.Error(codes.InvalidArgument, "IP is empty, must be provided")
	}
	if parameters.PoolName == "" || parameters.PoolName == "defaultPoolNameValue" {
		logger.Error("NewDriver(): PoolName is empty, must be provided")
		return nil, status.Error(codes.InvalidArgument, "PoolName is empty, must be provided")
	}
	if parameters.ApiKey == "" || parameters.ApiKey == "defaultApiKeyValue" {
		logger.Error("NewDriver(): ApiKey is empty, must be provided")
		return nil, status.Error(codes.InvalidArgument, "ApiKey is empty, must be provided")
	}
	if parameters.Endpoint == "" || parameters.Endpoint == "defaultEndpointValue" {
		logger.Error("NewDriver(): Endpoint is empty, must be provided")
		return nil, status.Error(codes.InvalidArgument, "Endpoint is empty, must be provided")
	}
	if parameters.Role == "" {
		parameters.Role = "node"
	}
	if parameters.Role != "node" && parameters.Role != "controller" && parameters.Role != "all" {
		logger.Error("NewDriver(): Role must be node, controller, or all",
			zap.String("role", parameters.Role),
		)
		return nil, status.Error(codes.InvalidArgument, "Role must be node, controller, or all")
	}
	if (parameters.Role == "node" || parameters.Role == "all") && (parameters.NodeId == "" || parameters.NodeId == "defaultNodeIDValue") {
		logger.Error("NewDriver(): NodeID is empty, must be provided")
		return nil, status.Error(codes.InvalidArgument, "NodeID is empty, must be provided")
	}

	// The portal group is optional; when empty the driver selects the first
	// available portal group at publish time.
	logger.Info("NewDriver(): portal group configured",
		zap.String("portal_group", parameters.PortalGroup),
	)

	// Construct the unified NGX SDK client. The API key is never logged.
	cfg := ngxstorage.Config{
		Controllers: strings.Split(parameters.Ip, ","),
		APIKey:      parameters.ApiKey,
		PoolName:    parameters.PoolName,
		// NGX appliances use self-signed certificates; skip verification by
		// default.
		InsecureSkipVerify: true,
		Logger:             logutils.NewSDKLogger(logger),
	}
	if injectedClient != nil {
		cfg.HTTPClient = injectedClient
	}
	client, err := ngxstorage.NewClient(cfg)
	if err != nil {
		logger.Error("NewDriver(): storage client creation failed",
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "create storage client: %v", err)
	}
	logger.Info("NewDriver(): storage client created",
		zap.String("pool_name", parameters.PoolName),
		zap.String("controller_ips", parameters.Ip),
	)

	// Resolve the active controller and configured pool during startup.
	initializationContext, cancelInitialization := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelInitialization()
	if err := client.RefreshController(initializationContext); err != nil {
		logger.Error("NewDriver(): controller and pool initialization failed",
			zap.Error(err),
		)
		code := codes.Unavailable
		if errors.Is(err, ngxstorage.ErrPoolNotFound) {
			code = codes.NotFound
		}
		return nil, status.Errorf(code, "driver initialization: %v", err)
	}
	logger.Info("NewDriver(): controller and pool initialized")

	// Resolve the pool's controller owner once at startup. Every target and
	// portal group must live on the same controller as the pool (the backend
	// rejects LUN mappings across controllers), so ownership is validated
	// here and reused for all subsequent placement decisions. The pool list
	// already carries the owner; the detail endpoint resolves by pool ID, so
	// the list is used directly.
	pools, err := client.Pools().List(initializationContext)
	if err != nil {
		logger.Error("NewDriver(): pool list failed",
			zap.String("pool_name", parameters.PoolName),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Unavailable, "driver initialization: list pools: %v", err)
	}
	var pool *ngxstorage.Pool
	for i := range pools {
		if pools[i].Name == parameters.PoolName {
			pool = &pools[i]
			break
		}
	}
	if pool == nil {
		logger.Error("NewDriver(): configured pool not found in pool list",
			zap.String("pool_name", parameters.PoolName),
		)
		return nil, status.Errorf(codes.NotFound, "driver initialization: pool not found: %q", parameters.PoolName)
	}
	logger.Info("NewDriver(): pool owner resolved",
		zap.String("pool_name", parameters.PoolName),
		zap.String("pool_owner", pool.Owner),
	)

	// Validate that the portal group serving the pool's LUNs is owned by the
	// same controller.
	portalGroups, err := client.PortalGroups().List(initializationContext)
	if err != nil {
		logger.Error("NewDriver(): portal group list failed", zap.Error(err))
		return nil, status.Errorf(codes.Unavailable, "driver initialization: list portal groups: %v", err)
	}
	var portalOwner string
	for i := range portalGroups {
		if parameters.PortalGroup != "" && portalGroups[i].Name != parameters.PortalGroup {
			continue
		}
		portalOwner = portalGroups[i].Owner
		break
	}
	if portalOwner == "" {
		logger.Error("NewDriver(): portal group not found",
			zap.String("portal_group", parameters.PortalGroup),
		)
		return nil, status.Errorf(codes.NotFound, "driver initialization: portal group not found: %q", parameters.PortalGroup)
	}
	if pool.Owner != portalOwner {
		logger.Error("NewDriver(): pool owner and portal group owner must be same controller",
			zap.String("pool_owner", pool.Owner),
			zap.String("portal_owner", portalOwner),
		)
		return nil, status.Errorf(codes.FailedPrecondition, "driver initialization: pool owner and portal group owner must be same controller")
	}
	logger.Info("NewDriver(): ownership check passed",
		zap.String("pool_owner", pool.Owner),
		zap.String("portal_owner", portalOwner),
	)

	// Build local CSI state after backend placement is known.
	d := &Driver{
		logger: logger,
		NodeId: parameters.NodeId,
		Role:   parameters.Role,

		Endpoint: parameters.Endpoint,
		client:   client,

		portalGroup: parameters.PortalGroup,
		poolOwner:   pool.Owner,

		Name:    ResolveDriverName(parameters.DriverPrefix),
		Version: DriverVersion,
		Ready:   false,

		mountPermissions: 0o750,
		sessionStateDir:  "/csi/session-state",
	}

	d.mounter = mount.New("")
	if d.bindMount == nil {
		d.bindMount = blockmount.Mounter.Mount
	}
	if d.nodeChmod == nil {
		d.nodeChmod = os.Chmod
	}
	// Kubernetes VolumeAttachment is the authoritative shared-publish registry.
	// Outside a cluster the dependency may be unavailable; destructive unpublish
	// then fails closed instead of deleting a target based on backend login timing.
	if parameters.Role != "node" && d.attachmentLookup == nil {
		lookup, err := kubeutils.NewAttachmentLookup()
		if err != nil {
			logger.Warn("NewDriver(): controller attachment lookup unavailable; unpublish will fail closed",
				zap.Error(err))
		} else {
			d.attachmentLookup = lookup
		}
	}

	// Materialize capability sets.
	d.nsCap = make([]*csi.NodeServiceCapability, 0, len(supportedNodeCapabilities))
	for _, capability := range supportedNodeCapabilities {
		logger.Info("NewDriver(): node capability configured",
			zap.String("capability", capability.String()),
		)
		d.nsCap = append(d.nsCap, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: capability},
			},
		})
	}
	d.csCap = make([]*csi.ControllerServiceCapability, 0, len(supportedControllerCapabilities))
	for _, capability := range supportedControllerCapabilities {
		logger.Info("NewDriver(): controller capability configured",
			zap.String("capability", capability.String()),
		)
		d.csCap = append(d.csCap, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: capability},
			},
		})
	}
	d.AccessModes = append([]csi.VolumeCapability_AccessMode_Mode(nil), supportedAccessModes...)
	for _, mode := range d.AccessModes {
		logger.Info("NewDriver(): access mode configured",
			zap.String("mode", mode.String()),
		)
	}
	logger.Info("NewDriver(): driver initialized",
		zap.String("name", d.Name),
		zap.String("version", d.Version),
		zap.String("endpoint", d.Endpoint),
		zap.String("node_id", d.NodeId),
	)
	return d, nil
}

/*
 * Run creates the configured unix socket, registers the identity service and
 * exactly one node or controller service, then blocks in gRPC Serve.
 *
 * @return error: Invalid endpoint/scheme, socket preparation failure, or
 *         the terminal gRPC Serve error.
 */
func (d *Driver) Run() error {
	logger := d.log()
	logger.Info("Run(): init")
	logger.Debug("Run(): configuration",
		zap.String("endpoint", d.Endpoint),
		zap.String("node_id", d.NodeId),
		zap.String("driver_name", d.Name),
		zap.String("driver_version", d.Version),
	)
	if err := d.prepareCoordination(context.Background()); err != nil {
		logger.Error("Run(): coordination preflight failed", zap.Error(err))
		return fmt.Errorf("coordination preflight: %w", err)
	}

	parsedEndpoint, err := url.Parse(d.Endpoint)
	if err != nil {
		logger.Error("Run(): endpoint parse failed",
			zap.String("endpoint", d.Endpoint),
			zap.Error(err),
		)
		return fmt.Errorf("invalid endpoint: %w", err)
	}
	if parsedEndpoint.Scheme != "unix" {
		logger.Error("Run(): endpoint scheme is unsupported",
			zap.String("scheme", parsedEndpoint.Scheme),
		)
		return fmt.Errorf("invalid endpoint scheme: %s", parsedEndpoint.Scheme)
	}
	socketPath := path.Join(parsedEndpoint.Host, filepath.FromSlash(parsedEndpoint.Path))
	if parsedEndpoint.Host == "" {
		socketPath = filepath.FromSlash(parsedEndpoint.Path)
	}
	socketDirectory := filepath.Dir(socketPath)
	if err := os.MkdirAll(socketDirectory, 0o750); err != nil {
		logger.Error("Run(): CSI socket directory creation failed",
			zap.String("directory", socketDirectory),
			zap.Error(err),
		)
		return fmt.Errorf("create CSI socket directory: %w", err)
	}

	err = os.Remove(socketPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing CSI socket: %w", err)
	}

	listener, err := net.Listen(parsedEndpoint.Scheme, socketPath)
	if err != nil {
		return fmt.Errorf("create CSI listener: %w", err)
	}

	d.Server = grpc.NewServer()
	csi.RegisterIdentityServer(d.Server, d)
	switch d.Role {
	case "controller":
		csi.RegisterControllerServer(d.Server, d)
	case "node":
		csi.RegisterNodeServer(d.Server, d)
	case "all":
		csi.RegisterNodeServer(d.Server, d)
		csi.RegisterControllerServer(d.Server, d)
	}

	d.Ready = true
	logger.Info("Run(): CSI service is ready",
		zap.Bool("ready", d.Ready),
		zap.String("socket_path", socketPath),
	)

	if err := d.Server.Serve(listener); err != nil {
		return err
	}
	return nil
}

func (d *Driver) prepareCoordination(ctx context.Context) error {
	if d.coordination == nil {
		coordinator, err := coord.NewCoordinatorFromEnvironment(d.Role)
		if err != nil {
			return err
		}
		d.coordination = coordinator
	}
	return coord.PreflightCoordinator(ctx, d.coordination)
}
