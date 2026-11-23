package ngxstorage

import (
	"encoding/json"
	"fmt"
)

// This file declares the canonical backend types returned by the NGX Storage
// Manager API v2. Field names mirror the live backend JSON keys.

// LUN is a block volume on the NGX Storage backend.
type LUN struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Size            FlexBytes `json:"size"` // bytes; number or string
	Used            FlexBytes `json:"used"`
	ScsiID          string    `json:"scsi_id"`
	BlockSize       string    `json:"blocksize"`
	ThinProvision   FlexOnOff `json:"thin_provision"`
	Deduplication   FlexOnOff `json:"deduplication"`
	Compression     FlexOnOff `json:"compression"`
	CompressRatio   string    `json:"compress_ratio"`
	DramCache       FlexOnOff `json:"dram_cache"`
	FlashCache      FlexOnOff `json:"flash_cache"`
	IoType          string    `json:"io_type"`
	QosPriority     FlexBytes `json:"qos_priority"` // number or string
	Clone           string    `json:"clone"`
	SizeBySnapshots FlexBytes `json:"sizebysnapshots"`
	Owner           string    `json:"owner"`
	PoolName        string    `json:"pool_name"`
	// Created is the creation timestamp string (e.g. 2025-11-24T08:50:44Z).
	Created string `json:"created"`
	// Protocols maps protocol name to its target reference, e.g. {"fc": "tgtA"}.
	Protocols map[string]string `json:"protocols"`
}

// Share is a file (NFS) volume on the NGX Storage backend. Field names mirror
// the live backend JSON of GET /api/v2/share/{id} and GET /api/v2/share/list.
type Share struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	PoolName string `json:"pool_name"`
	Owner    string `json:"owner"`
	// Created is the creation timestamp; absent or zero quotas/timestamps are
	// serialized by the backend as null.
	Created  *FlexBytes `json:"created"`
	ShareIPs []string   `json:"share_ips"`
	// Capacity nests the share capacity and quota byte values.
	Capacity ShareCapacity `json:"capacity"`
	// Storage nests the storage attributes (blocksize, caches, permissions).
	Storage ShareStorage `json:"storage"`
	// Exports nests the canonical NFS export state.
	Exports ShareExports `json:"exports"`
}

// UnmarshalJSON decodes a share with the exact strictness each document
// requires:
//
//   - identity and storage attributes are lenient: type-invalid values decode
//     to their zero values so one malformed record inside a list response does
//     not hide the remaining records;
//   - capacity quotas are nullable pointers: a null or malformed quota decodes
//     to nil and callers that require a quota reject it explicitly;
//   - exports.nfs.enabled and exports.nfs.read_only are strict: a present but
//     malformed export state is an error, because silently treating a corrupt
//     export as disabled would publish or unpublish the wrong share mode.
func (s *Share) UnmarshalJSON(data []byte) error {
	*s = Share{}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decodeLenientJSONField(fields, "id", &s.ID)
	decodeLenientJSONField(fields, "name", &s.Name)
	decodeLenientJSONField(fields, "pool_name", &s.PoolName)
	decodeLenientJSONField(fields, "owner", &s.Owner)
	decodeLenientJSONField(fields, "created", &s.Created)
	decodeLenientJSONField(fields, "share_ips", &s.ShareIPs)

	var capacity map[string]json.RawMessage
	if raw := fields["capacity"]; len(raw) != 0 && json.Unmarshal(raw, &capacity) == nil {
		decodeLenientJSONField(capacity, "used", &s.Capacity.Used)
		decodeLenientJSONField(capacity, "data", &s.Capacity.Data)
		decodeLenientJSONField(capacity, "snapshots", &s.Capacity.Snapshots)
		decodeLenientJSONField(capacity, "hard_quota", &s.Capacity.HardQuota)
		decodeLenientJSONField(capacity, "soft_quota", &s.Capacity.SoftQuota)
		decodeLenientJSONField(capacity, "reserved", &s.Capacity.Reserved)
	}

	var storage map[string]json.RawMessage
	if raw := fields["storage"]; len(raw) != 0 && json.Unmarshal(raw, &storage) == nil {
		decodeLenientJSONField(storage, "blocksize", &s.Storage.BlockSize)
		decodeLenientJSONField(storage, "deduplication", &s.Storage.Deduplication)
		decodeLenientJSONField(storage, "compression", &s.Storage.Compression)
		decodeLenientJSONField(storage, "compress_ratio", &s.Storage.CompressRatio)
		decodeLenientJSONField(storage, "provision_type", &s.Storage.ProvisionType)
		decodeLenientJSONField(storage, "dram_cache", &s.Storage.DramCache)
		decodeLenientJSONField(storage, "flash_cache", &s.Storage.FlashCache)
		decodeLenientJSONField(storage, "io_type", &s.Storage.IOType)
		decodeLenientJSONField(storage, "permissions", &s.Storage.Permissions)
		decodeLenientJSONField(storage, "flashtier_limit", &s.Storage.FlashTierLimit)
		decodeLenientJSONField(storage, "clone", &s.Storage.Clone)
	}

	if rawExports := fields["exports"]; len(rawExports) != 0 {
		var raw struct {
			NFS struct {
				Enabled  json.RawMessage `json:"enabled"`
				ReadOnly json.RawMessage `json:"read_only"`
			} `json:"nfs"`
		}
		if err := json.Unmarshal(rawExports, &raw); err != nil {
			return err
		}
		if len(raw.NFS.Enabled) != 0 {
			enabled, err := flexBoolFromValue(decodeJSONValue(raw.NFS.Enabled))
			if err != nil {
				return fmt.Errorf("ngxstorage: share %q export enabled: %w", s.ID, err)
			}
			s.Exports.NFS.Enabled = enabled
		}
		if len(raw.NFS.ReadOnly) != 0 {
			readOnly, err := flexBoolFromValue(decodeJSONValue(raw.NFS.ReadOnly))
			if err != nil {
				return fmt.Errorf("ngxstorage: share %q export read_only: %w", s.ID, err)
			}
			s.Exports.NFS.ReadOnly = readOnly
		}
	}
	return nil
}

func decodeLenientJSONField[T any](fields map[string]json.RawMessage, name string, destination *T) {
	if raw := fields[name]; len(raw) != 0 {
		var decoded T
		if json.Unmarshal(raw, &decoded) == nil {
			*destination = decoded
		}
	}
}

// ShareCapacity is the nested capacity document of a share. The backend
// serializes quota fields as null when unset, so they decode to nil pointers.
type ShareCapacity struct {
	Used      FlexBytes  `json:"used"`
	Data      FlexBytes  `json:"data"`
	Snapshots FlexBytes  `json:"snapshots"`
	HardQuota *FlexBytes `json:"hard_quota"`
	SoftQuota *FlexBytes `json:"soft_quota"`
	Reserved  *FlexBytes `json:"reserved"`
}

// ShareStorage is the nested storage-attributes document of a share.
type ShareStorage struct {
	BlockSize     string   `json:"blocksize"`
	Deduplication FlexBool `json:"deduplication"`
	Compression   FlexBool `json:"compression"`
	CompressRatio *string  `json:"compress_ratio"`
	ProvisionType string   `json:"provision_type"` // "thin" | "full"
	DramCache     FlexBool `json:"dram_cache"`
	FlashCache    FlexBool `json:"flash_cache"`
	IOType        string   `json:"io_type"` // "transactional" | "sequential"
	// Permissions is the 9-bit binary rwx string (e.g. "111111111" = 777).
	Permissions string `json:"permissions"`
	// FlashTierLimit is the flashtier blocksize limit ("Nk", "512", or "0").
	FlashTierLimit string  `json:"flashtier_limit"`
	Clone          *string `json:"clone"`
}

// ShareExports is the nested export document of a share.
type ShareExports struct {
	NFS ShareNFSExport `json:"nfs"`
}

// ShareNFSExport is the NFS export state of a share.
type ShareNFSExport struct {
	// Enabled follows the live backend representation: "1" enabled, ""
	// disabled; FlexBool also accepts JSON booleans from other releases.
	Enabled FlexBool `json:"enabled"`
	// ReadOnly reports whether the export is read-only.
	ReadOnly FlexBool `json:"read_only"`
}

// FCTarget is a Fibre Channel target on the backend.
type FCTarget struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Owner string        `json:"owner"`
	Ports []FCPort      `json:"ports"`
	Luns  []FCTargetLUN `json:"luns"`
}

// FCPort is a physical FC adapter port.
type FCPort struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Wwpns      []FCWWPN      `json:"wwpns"`
	Initiators []FCInitiator `json:"initiators"`
}

// FCWWPN is a World Wide Port Name.
type FCWWPN struct {
	WWPN     string `json:"wwpn"`      // e.g. "naa.20020090faeae1a5"
	Owner    string `json:"owner"`     // controller product ID
	TargetID string `json:"target_id"` // FC target ID this WWPN is assigned to
}

// FCInitiator is a logged-in initiator on an FC port.
type FCInitiator struct {
	NAA string `json:"naa"`
	Tag string `json:"tag"`
}

// FCTargetLUN is a LUN mapped to an FC target.
type FCTargetLUN struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Number   string `json:"number"` // SCSI LUN number, string on the wire
	PoolName string `json:"pool_name"`
	ScsiID   string `json:"scsi_id"`
}

// FCPortInfo is a hardware FC adapter port from /api/v2/target/fc/ports.
type FCPortInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Number        int      `json:"number"`
	Available     bool     `json:"available"`
	AdapterSerial string   `json:"adapter_serial"`
	Wwpns         []FCWWPN `json:"wwpns"`
}

// ISCSITarget is an iSCSI target on the backend.
type ISCSITarget struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Owner           string                 `json:"owner"`
	TargetIQN       string                 `json:"target_iqn"`
	AuthGroupID     string                 `json:"auth_group_id"`
	AuthGroupName   string                 `json:"auth_group_name"`
	PortalGroupID   string                 `json:"portal_group_id"`
	PortalGroupName string                 `json:"portal_group_name"`
	Initiators      []ISCSITargetInitiator `json:"initiators"`
	Luns            []ISCSITargetLUN       `json:"luns"`
}

// ISCSITargetInitiator is a logged-in iSCSI initiator of a target.
type ISCSITargetInitiator struct {
	ID    string `json:"id"`
	IQN   string `json:"iqn"`
	Alias string `json:"alias"`
	IP    string `json:"ip"`
}

// ISCSITargetLUN is a LUN mapped to an iSCSI target.
type ISCSITargetLUN struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Number   string `json:"number"` // SCSI LUN number; number or string on the wire
	PoolName string `json:"pool_name"`
	ScsiID   string `json:"scsi_id"`
}

// Initiator is an iSCSI initiator (IQN) or FC initiator (WWPN) currently
// connected to the system. iSCSI entries carry the iqn, source ip, auth-group
// alias, and the target iqn they are logged into; FC entries carry the
// initiator WWPN and the target port.
type Initiator struct {
	ID        string `json:"id,omitempty"`
	IQN       string `json:"iqn,omitempty"`
	Alias     string `json:"alias,omitempty"`
	NAA       string `json:"naa,omitempty"`
	Initiator string `json:"initiator,omitempty"`
	Port      string `json:"port,omitempty"`
	PortNAA   string `json:"port_naa,omitempty"`
	Tag       string `json:"tag,omitempty"`
	IP        string `json:"ip,omitempty"`
	TargetIQN string `json:"target_iqn,omitempty"`
	Type      string `json:"type,omitempty"`
}

// AuthGroup is an iSCSI auth group (CHAP credential container).
type AuthGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ChapStatus is an integer (0/1) on current software; FlexBool also
	// accepts the JSON booleans older releases return.
	ChapStatus FlexBool    `json:"chap_status"`
	Initiators []Initiator `json:"initiator"`
}

// PortalGroupListenIP is one listen address (interface and IP) of a portal
// group detail record.
type PortalGroupListenIP struct {
	ID            string `json:"id"`
	InterfaceName string `json:"interface_name"`
	IPAddress     string `json:"ip_address"`
	Owner         string `json:"owner"`
}

// PortalGroup is an iSCSI portal group exposing target IPs.
type PortalGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Owner is present on lightweight list entries.
	Owner string `json:"owner"`
	// ListenIPs is present only on the detail endpoint; the lightweight list
	// omits it.
	ListenIPs []PortalGroupListenIP `json:"listen_ips"`
}

// IPAddresses flattens the listen_ips records to their ip_address values.
func (p PortalGroup) IPAddresses() []string {
	ips := make([]string, 0, len(p.ListenIPs))
	for _, lip := range p.ListenIPs {
		if lip.IPAddress != "" {
			ips = append(ips, lip.IPAddress)
		}
	}
	return ips
}

// Snapshot is a point-in-time copy of a LUN or share. Field names mirror the
// live backend JSON; the SDK does not probe alternate names.
type Snapshot struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	VolumeID   string `json:"volume_id"`
	VolumeName string `json:"volume_name"`
	PoolName   string `json:"pool_name"`
	Path       string `json:"path"`
	VolumeType string `json:"volume_type"`
	Size       string `json:"reserved"` // bytes, string on the wire
	Created    string `json:"created"`
	Ready      bool   `json:"ready"`
}

// UnmarshalJSON decodes a snapshot record. NGX snapshots are synchronous
// point-in-time copies: the backend (verified live on V6BGWYPA and SGZ1RVC2,
// create and get) returns no `ready` field at all, and a fetched record is
// usable immediately. Ready therefore defaults to true and is only cleared
// by an explicit wire `ready` value (boolean or 0/1 number). Without this,
// every snapshot would report ReadyToUse=false forever and no
// VolumeSnapshot could ever reach readyToUse.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	// The wire struct carries every field except Ready so a non-boolean
	// wire encoding can never fail the whole decode; Ready is resolved
	// explicitly below.
	var raw struct {
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		VolumeID   string          `json:"volume_id"`
		VolumeName string          `json:"volume_name"`
		PoolName   string          `json:"pool_name"`
		Path       string          `json:"path"`
		VolumeType string          `json:"volume_type"`
		Size       string          `json:"reserved"`
		Created    string          `json:"created"`
		Ready      json.RawMessage `json:"ready"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.ID, s.Name = raw.ID, raw.Name
	s.VolumeID, s.VolumeName = raw.VolumeID, raw.VolumeName
	s.PoolName, s.Path = raw.PoolName, raw.Path
	s.VolumeType, s.Size = raw.VolumeType, raw.Size
	s.Created = raw.Created
	s.Ready = true
	if len(raw.Ready) != 0 && string(raw.Ready) != "null" {
		var b bool
		if err := json.Unmarshal(raw.Ready, &b); err == nil {
			s.Ready = b
		} else {
			var n int
			if err := json.Unmarshal(raw.Ready, &n); err != nil {
				return fmt.Errorf("decode snapshot ready: %w", err)
			}
			s.Ready = n != 0
		}
	}
	return nil
}

// Pool is a storage pool on the backend.
type Pool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	IsFlash   bool   `json:"is_flash"`
	BlockSize string `json:"blocksize"`
}

// UnmarshalJSON accepts the canonical pool fields. The backend encodes
// is_flash as either a JSON boolean or a 0/1 number depending on endpoint.
func (p *Pool) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Owner     string          `json:"owner"`
		IsFlash   json.RawMessage `json:"is_flash"`
		BlockSize string          `json:"blocksize"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.ID, p.Name = raw.ID, raw.Name
	p.Owner, p.BlockSize = raw.Owner, raw.BlockSize
	if len(raw.IsFlash) != 0 && string(raw.IsFlash) != "null" {
		var b bool
		if err := json.Unmarshal(raw.IsFlash, &b); err == nil {
			p.IsFlash = b
		} else {
			var n int
			if err := json.Unmarshal(raw.IsFlash, &n); err != nil {
				return fmt.Errorf("decode pool is_flash: %w", err)
			}
			p.IsFlash = n != 0
		}
	}
	return nil
}

// SnapshotSchedule is the snapshot schedule installed on a volume
// (GET/POST/DELETE /api/v2/snapshot/schedule/{volume_id}). The wire document
// is the backend's schedules-table row: an empty schedule reads back with
// empty strings. Creating a schedule sends cron_time plus keep_max.
type SnapshotSchedule struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	HumanRead  string `json:"h_read"`
	CronTime   string `json:"c_time"`
	KeepMax    string `json:"keep_max"`
	VolumeID   string `json:"volume_id"`
	VolumeType string `json:"volume_type"` // "share" | "lun"
}

// FCInitiatorTag is an FC initiator tag (zoning alias) for one initiator NAA
// (GET/POST /api/v2/target/fc/initiator/tag).
type FCInitiatorTag struct {
	ID           string `json:"id"`
	InitiatorNAA string `json:"initiator_naa"`
	Tag          string `json:"tag"`
	Owner        string `json:"owner"`
}

// Peer is a cluster member in the cluster status response.
type Peer struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ClusterStatus is the response of GET /api/v2/status/cluster.
type ClusterStatus struct {
	Connected int    `json:"connected"`
	Status    string `json:"status"` // Master | Ready | Cluster
	Peers     []Peer `json:"peers"`
}

// mutationResponse is the canonical acknowledgement for LUN/share/snapshot
// mutations.
type mutationResponse struct {
	Success successFlag `json:"success"`
}
