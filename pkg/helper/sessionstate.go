package helper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type ISCSISessionState struct {
	TargetIQN string   `json:"target_iqn"`
	Portals   []string `json:"portals"`
	SCSIID    string   `json:"scsi_id,omitempty"`
}

func SessionStatePath(dir, volumeID string) string {
	digest := sha256.Sum256([]byte(volumeID))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
}

func SaveISCSISessionState(dir, volumeID string, state ISCSISessionState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session state directory: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}
	path := SessionStatePath(dir, volumeID)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return fmt.Errorf("write session state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("commit session state: %w", err)
	}
	return nil
}

func LoadISCSISessionState(dir, volumeID string) (ISCSISessionState, error) {
	raw, err := os.ReadFile(SessionStatePath(dir, volumeID))
	if err != nil {
		return ISCSISessionState{}, err
	}
	var state ISCSISessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return ISCSISessionState{}, fmt.Errorf("decode session state: %w", err)
	}
	return state, nil
}

func DeleteISCSISessionState(dir, volumeID string) error {
	err := os.Remove(SessionStatePath(dir, volumeID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
