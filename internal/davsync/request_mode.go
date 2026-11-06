package davsync

import "github.com/yetone/magpie/internal/backup"

// withoutRequestMode removes this computer's request policy from an upload.
// A copy keeps the local bundle, also used for Undo, intact.
func withoutRequestMode(b backup.Bundle) backup.Bundle {
	if b.Settings != nil {
		s := *b.Settings
		s.RequestMode = ""
		b.Settings = &s
	}
	return b
}
