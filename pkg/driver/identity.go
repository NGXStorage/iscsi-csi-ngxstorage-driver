package driver

import (
	"context"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	zap "go.uber.org/zap"
	status "google.golang.org/grpc/status"
	wrapperspb "google.golang.org/protobuf/types/known/wrapperspb"
)

/*
 * GetPluginInfo returns the configured CSI driver name and vendor version.
 * It performs no NGX request, filesystem mutation, or capability negotiation.
 *
 * @param ctx: Carries the CSI deadline and cancellation signal.
 * @param req: Empty CSI request.
 *
 * @return *csi.GetPluginInfoResponse: Driver name, version, and an empty
 *         manifest.
 * @return error: Canceled or DeadlineExceeded when the caller context ended.
 */
func (d *Driver) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	d.log().Info("GetPluginInfo(): init")
	d.log().Debug("GetPluginInfo(): request",
		zap.Bool("request_present", req != nil),
	)

	// Stop before constructing identity state when the CSI request ended.
	if err := ctx.Err(); err != nil {
		d.log().Error("GetPluginInfo(): context ended", zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}

	// Identity values come from immutable driver configuration and contain no
	// backend credentials or node-specific state.
	response := &csi.GetPluginInfoResponse{
		Name:          d.Name,
		VendorVersion: d.Version,
		Manifest:      map[string]string{},
	}
	d.log().Info("GetPluginInfo(): plugin identity returned",
		zap.String("name", response.GetName()),
		zap.String("vendor_version", response.GetVendorVersion()),
	)
	return response, nil
}

/*
 * GetPluginCapabilities returns the CSI services implemented by this driver.
 * NGX Storage iSCSI exports are reachable from every eligible worker node, so the
 * driver does not advertise topology constraints or invent topology segments.
 * The operation performs no storage or filesystem request.
 *
 * @param ctx: Carries the CSI deadline and cancellation signal.
 * @param req: Empty CSI request.
 *
 * @return *csi.GetPluginCapabilitiesResponse: Returns controller service and
 *         online expansion support consumed by the deployed external-resizer.
 * @return error: Context cancellation/deadline status.
 */
func (d *Driver) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	d.log().Info("GetPluginCapabilities(): init")
	d.log().Debug("GetPluginCapabilities(): request",
		zap.Bool("request_present", req != nil),
	)

	// Stop before constructing a response when the caller's deadline ended.
	if err := ctx.Err(); err != nil {
		d.log().Error("GetPluginCapabilities(): context ended", zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}

	// The deployment includes an external-resizer. The driver advertises the
	// controller service and online volume expansion capabilities.
	response := &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
		},
	}
	d.log().Info("GetPluginCapabilities(): capabilities returned",
		zap.Reflect("capabilities", response.GetCapabilities()),
	)
	return response, nil
}

/*
 * Probe reports whether the CSI gRPC service completed initialization.
 * NewDriver has already resolved the configured NGX controller and pool before
 * Run starts the listener; Run sets Ready only after service registration.
 * Probe itself performs no backend request or filesystem mutation.
 *
 * @param ctx: Carries the CSI deadline and cancellation signal.
 * @param req: Empty CSI request.
 *
 * @return *csi.ProbeResponse: Ready contains the current process readiness.
 * @return error: Canceled or DeadlineExceeded when the caller context ended.
 */
func (d *Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	d.log().Info("Probe(): init")
	d.log().Debug("Probe(): request",
		zap.Bool("request_present", req != nil),
	)

	// Honor the caller's deadline before reading process readiness.
	if err := ctx.Err(); err != nil {
		d.log().Error("Probe(): context ended", zap.Error(err))
		return nil, status.FromContextError(err).Err()
	}

	response := &csi.ProbeResponse{
		Ready: &wrapperspb.BoolValue{
			Value: d.Ready,
		},
	}
	d.log().Info("Probe(): readiness returned",
		zap.Bool("ready", response.GetReady().GetValue()),
	)
	return response, nil
}
