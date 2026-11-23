package ngxstorage

import (
	"net/http"
	"net/url"
)

const (
	apiVersion     = "api/v2"
	lunGroup       = "lun"
	shareGroup     = "share"
	fcTargetGroup  = "target/fc"
	iscsiGroup     = "target/iscsi"
	snapshotGroup  = "snapshot"
	poolGroup      = "pool"
	statusGroup    = "status"
	authGroupGroup = "auth_group"
	portalGroupGrp = "portal_group"
	initiatorGroup = "initiators"
	hardwareGroup  = "hardware"
	servicesGroup  = "services"
	logGroup       = "log"
	hostGroup      = "host"
	userGroup      = "user"
	networkGroup   = "network"
	infoGroup      = "info"
)

// endpoint describes one declarative NGX API v2 endpoint.
type endpoint struct {
	Group         string
	Method        string
	Segments      []string
	PathVars      url.Values
	TrailingSlash bool
}

// endpoints is the declarative map of all NGX API v2 endpoints used by the
// SDK. Adding a new operation is a single map entry; URL construction and
// path-variable substitution are handled by endpointURL. Seven entries
// (ChangeAuthGroup, CreateFCTarget, UpdateFCTarget, DeleteFCTarget, GetFCPorts,
// GetLUNList, GetPoolsStatus) are declared for API-surface coverage but no
// service method uses them yet.
var endpoints = map[string]endpoint{
	// LUN
	"CreateLUN":        {Group: lunGroup, Method: http.MethodPost, TrailingSlash: true},
	"GetLUN":           {Group: lunGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteLUN":        {Group: lunGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"ModifyLUN":        {Group: lunGroup, Method: http.MethodPatch, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetLUNList":       {Group: lunGroup, Method: http.MethodGet},
	"GetLUNDetailList": {Group: lunGroup, Method: http.MethodGet, Segments: []string{"list"}},

	// Share (NFS)
	"CreateShare":        {Group: shareGroup, Method: http.MethodPost, TrailingSlash: true},
	"GetShare":           {Group: shareGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteShare":        {Group: shareGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"ModifyShare":        {Group: shareGroup, Method: http.MethodPatch, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetShareList":       {Group: shareGroup, Method: http.MethodGet},
	"GetShareDetailList": {Group: shareGroup, Method: http.MethodGet, Segments: []string{"list"}},

	// FC target
	"GetFCTargetList":       {Group: fcTargetGroup, Method: http.MethodGet},
	"GetFCTargetDetailList": {Group: fcTargetGroup, Method: http.MethodGet, Segments: []string{"list"}},
	"GetFCTarget":           {Group: fcTargetGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"CreateFCTarget":        {Group: fcTargetGroup, Method: http.MethodPost},
	"UpdateFCTarget":        {Group: fcTargetGroup, Method: http.MethodPut, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteFCTarget":        {Group: fcTargetGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetFCPorts":            {Group: fcTargetGroup, Method: http.MethodGet, Segments: []string{"ports"}},
	"AddLunToFCTarget":      {Group: fcTargetGroup, Method: http.MethodPut, Segments: []string{"lun", "{id}"}, PathVars: url.Values{"id": {}}},
	"RemoveLunFromFCTarget": {Group: fcTargetGroup, Method: http.MethodPatch, Segments: []string{"lun", "{id}"}, PathVars: url.Values{"id": {}}},
	"GetFCInitiatorTagList": {Group: fcTargetGroup, Method: http.MethodGet, Segments: []string{"initiator", "tag"}},
	"CreateFCInitiatorTag":  {Group: fcTargetGroup, Method: http.MethodPost, Segments: []string{"initiator", "tag"}},
	"GetFCInitiatorTag":     {Group: fcTargetGroup, Method: http.MethodGet, Segments: []string{"initiator", "tag", "{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteFCInitiatorTag":  {Group: fcTargetGroup, Method: http.MethodDelete, Segments: []string{"initiator", "tag", "{id}"}, PathVars: url.Values{"id": {}}},

	// iSCSI target
	"GetISCSITargetList":           {Group: iscsiGroup, Method: http.MethodGet},
	"GetISCSITargetDetailList":     {Group: iscsiGroup, Method: http.MethodGet, Segments: []string{"list"}},
	"GetISCSITarget":               {Group: iscsiGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"CreateISCSITarget":            {Group: iscsiGroup, Method: http.MethodPost},
	"DeleteISCSITarget":            {Group: iscsiGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"ChangeISCSITargetName":        {Group: iscsiGroup, Method: http.MethodPatch, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"ChangeISCSITargetAuthGroup":   {Group: iscsiGroup, Method: http.MethodPatch, Segments: []string{"auth_group", "{id}"}, PathVars: url.Values{"id": {}}},
	"ChangeISCSITargetPortalGroup": {Group: iscsiGroup, Method: http.MethodPatch, Segments: []string{"portal_group", "{id}"}, PathVars: url.Values{"id": {}}},
	"AddLunToISCSITarget":          {Group: iscsiGroup, Method: http.MethodPut, Segments: []string{"lun", "{id}"}, PathVars: url.Values{"id": {}}},
	"RemoveLunFromISCSITarget":     {Group: iscsiGroup, Method: http.MethodPatch, Segments: []string{"lun", "{id}"}, PathVars: url.Values{"id": {}}},

	// Snapshot
	"CreateSnapshot":         {Group: snapshotGroup, Method: http.MethodPost},
	"GetSnapshot":            {Group: snapshotGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteSnapshot":         {Group: snapshotGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteAllSnapshots":     {Group: snapshotGroup, Method: http.MethodDelete, Segments: []string{"all", "{id}"}, PathVars: url.Values{"id": {}}},
	"GetSnapshotList":        {Group: snapshotGroup, Method: http.MethodGet},
	"GetSnapshotDetailList":  {Group: snapshotGroup, Method: http.MethodGet, Segments: []string{"list"}},
	"GetSnapshotSchedule":    {Group: snapshotGroup, Method: http.MethodGet, Segments: []string{"schedule", "{id}"}, PathVars: url.Values{"id": {}}},
	"CreateSnapshotSchedule": {Group: snapshotGroup, Method: http.MethodPost, Segments: []string{"schedule", "{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteSnapshotSchedule": {Group: snapshotGroup, Method: http.MethodDelete, Segments: []string{"schedule", "{id}"}, PathVars: url.Values{"id": {}}},
	"CloneSnapshot":          {Group: snapshotGroup, Method: http.MethodPost, Segments: []string{"clone", "{id}"}, PathVars: url.Values{"id": {}}},
	"RestoreSnapshot":        {Group: snapshotGroup, Method: http.MethodPost, Segments: []string{"restore", "{id}"}, PathVars: url.Values{"id": {}}},

	// Pool
	"GetPoolList":       {Group: poolGroup, Method: http.MethodGet},
	"GetPoolDetailList": {Group: poolGroup, Method: http.MethodGet, Segments: []string{"list"}},
	"GetPool":           {Group: poolGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetPoolOverview":   {Group: poolGroup, Method: http.MethodGet, Segments: []string{"overview"}},

	// Status
	"GetClusterStatus":     {Group: statusGroup, Method: http.MethodGet, Segments: []string{"cluster"}},
	"GetServicesStatus":    {Group: statusGroup, Method: http.MethodGet, Segments: []string{"services"}},
	"GetCapacityStatus":    {Group: statusGroup, Method: http.MethodGet, Segments: []string{"capacity"}},
	"GetPoolsStatus":       {Group: statusGroup, Method: http.MethodGet, Segments: []string{"pools"}},
	"GetIOPSStatus":        {Group: statusGroup, Method: http.MethodGet, Segments: []string{"iops"}},
	"GetBandwidth":         {Group: statusGroup, Method: http.MethodGet, Segments: []string{"bandwidth"}},
	"GetCacheStatus":       {Group: statusGroup, Method: http.MethodGet, Segments: []string{"cache"}},
	"GetCPUStatus":         {Group: statusGroup, Method: http.MethodGet, Segments: []string{"cpu"}},
	"GetEnclosuresStatus":  {Group: statusGroup, Method: http.MethodGet, Segments: []string{"enclosures"}},
	"GetLicenseStatus":     {Group: statusGroup, Method: http.MethodGet, Segments: []string{"license"}},
	"GetOverloadedVolumes": {Group: statusGroup, Method: http.MethodGet, Segments: []string{"overloaded_volumes"}},

	// Auth group (iSCSI CHAP)
	"CreateAuthGroup":        {Group: authGroupGroup, Method: http.MethodPost},
	"GetAuthGroup":           {Group: authGroupGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteAuthGroup":        {Group: authGroupGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetAuthGroupList":       {Group: authGroupGroup, Method: http.MethodGet},
	"GetAuthGroupDetailList": {Group: authGroupGroup, Method: http.MethodGet, Segments: []string{"list"}},
	"ChangeAuthGroup":        {Group: authGroupGroup, Method: http.MethodPatch, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"AddCHAP":                {Group: authGroupGroup, Method: http.MethodPatch, Segments: []string{"chap", "{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteCHAP":             {Group: authGroupGroup, Method: http.MethodDelete, Segments: []string{"chap", "{id}"}, PathVars: url.Values{"id": {}}},
	"AddIQN":                 {Group: authGroupGroup, Method: http.MethodPatch, Segments: []string{"iqn", "{id}"}, PathVars: url.Values{"id": {}}},
	"DeleteIQN":              {Group: authGroupGroup, Method: http.MethodDelete, Segments: []string{"iqn", "{id}"}, PathVars: url.Values{"id": {}}},

	// Portal group (iSCSI)
	"GetPortalGroupList":       {Group: portalGroupGrp, Method: http.MethodGet},
	"GetPortalGroupDetailList": {Group: portalGroupGrp, Method: http.MethodGet, Segments: []string{"list"}},
	"GetPortalGroup":           {Group: portalGroupGrp, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	// The deployed backend has no portal-group create/delete routes; these
	// entries exist only for the Deprecated compatibility methods below.
	"CreatePortalGroup": {Group: portalGroupGrp, Method: http.MethodPost},
	"DeletePortalGroup": {Group: portalGroupGrp, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},

	// Initiators
	"GetInitiators":         {Group: initiatorGroup, Method: http.MethodGet},
	"GetFCInitiators":       {Group: initiatorGroup, Method: http.MethodGet, Segments: []string{"fc", "{id}"}, PathVars: url.Values{"id": {}}},
	"GetFCInitiatorsAll":    {Group: initiatorGroup, Method: http.MethodGet, Segments: []string{"fc"}},
	"GetISCSIInitiators":    {Group: initiatorGroup, Method: http.MethodGet, Segments: []string{"iscsi", "{id}"}, PathVars: url.Values{"id": {}}},
	"GetISCSIInitiatorsAll": {Group: initiatorGroup, Method: http.MethodGet, Segments: []string{"iscsi"}},

	// Hardware
	"GetHardwareCPU":         {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"cpu", "list"}},
	"GetHardwareDisk":        {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"disk", "list"}},
	"GetHardwareFan":         {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"fan", "list"}},
	"GetHardwareMemory":      {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"memory", "list"}},
	"GetHardwareMemoryTotal": {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"memory", "total"}},
	"GetHardwarePowerSupply": {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"power_supply", "list"}},
	"GetHardwareEnclosure":   {Group: hardwareGroup, Method: http.MethodGet, Segments: []string{"enclosure", "list"}},

	// Services (NFS)
	"GetNFSService":     {Group: servicesGroup, Method: http.MethodGet, Segments: []string{"nfs", "status"}},
	"StartNFSService":   {Group: servicesGroup, Method: http.MethodPost, Segments: []string{"nfs", "start"}},
	"StopNFSService":    {Group: servicesGroup, Method: http.MethodPost, Segments: []string{"nfs", "stop"}},
	"RestartNFSService": {Group: servicesGroup, Method: http.MethodPost, Segments: []string{"nfs", "restart"}},
	"GetNFSSettings":    {Group: servicesGroup, Method: http.MethodGet, Segments: []string{"nfs", "settings"}},
	"SetNFSSettings":    {Group: servicesGroup, Method: http.MethodPut, Segments: []string{"nfs", "settings"}},

	// Log
	"GetAlertLog":       {Group: logGroup, Method: http.MethodGet, Segments: []string{"alert"}},
	"GetAuditLog":       {Group: logGroup, Method: http.MethodGet, Segments: []string{"audit"}},
	"GetSystemEventLog": {Group: logGroup, Method: http.MethodGet, Segments: []string{"system_event"}},

	// Host, User, Network, Info
	"GetHostList":    {Group: hostGroup, Method: http.MethodGet},
	"GetHost":        {Group: hostGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetUserList":    {Group: userGroup, Method: http.MethodGet},
	"GetUser":        {Group: userGroup, Method: http.MethodGet, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"CreateUser":     {Group: userGroup, Method: http.MethodPost},
	"DeleteUser":     {Group: userGroup, Method: http.MethodDelete, Segments: []string{"{id}"}, PathVars: url.Values{"id": {}}},
	"GetNetworkInfo": {Group: networkGroup, Method: http.MethodGet, Segments: []string{"info"}},
	"GetSystemInfo":  {Group: infoGroup, Method: http.MethodGet, Segments: []string{"system"}},
}

// endpointURL builds an absolute URL for one canonical endpoint name against
// the given controller IP. Path variables declared by the endpoint are
// substituted from pathVars; any remaining pathVars keys become query
// parameters (e.g. the snapshot ?volume_id= filter).
func endpointURL(name, ip string, pathVars url.Values) string {
	ep, ok := endpoints[name]
	if !ok {
		return ""
	}
	link, _ := url.JoinPath("https://"+ip, apiVersion, ep.Group)

	// Track which keys the endpoint consumes as path segments so they are not
	// also emitted as query parameters.
	pathKeys := map[string]bool{}
	for _, seg := range ep.Segments {
		if len(seg) > 0 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			key := seg[1 : len(seg)-1]
			pathKeys[key] = true
			if vals := pathVars[key]; len(vals) > 0 {
				seg = vals[0]
			}
		}
		link, _ = url.JoinPath(link, seg)
	}
	if ep.TrailingSlash {
		link += "/"
	}

	// Emit leftover pathVars keys as query parameters so callers can pass
	// filters (volume_id) without a second URL-building mechanism.
	query := url.Values{}
	for k, vs := range pathVars {
		if k == "" || pathKeys[k] {
			continue
		}
		for _, v := range vs {
			query.Add(k, v)
		}
	}
	if encoded := query.Encode(); encoded != "" {
		link += "?" + encoded
	}
	return link
}

func endpointMethod(name string) string {
	if ep, ok := endpoints[name]; ok {
		return ep.Method
	}
	return http.MethodGet
}

func endpointPathVars(name string) url.Values {
	ep := endpoints[name]
	dst := url.Values{}
	for k, v := range ep.PathVars {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}
