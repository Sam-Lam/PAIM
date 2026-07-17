package services

import (
	"time"

	"github.com/autolinepro/paim/internal/domain"
)

// Convenience aliases for the domain status constants the mapping code compares
// against most often, kept here so callers read cleanly.
const (
	domainVerified       = domain.VerificationStatusVerified
	domainBackupComplete = domain.BackupStatusComplete
)

// timeNow is the clock used by services when stamping timestamps. It is a
// package variable so tests may substitute a deterministic clock.
var timeNow = time.Now
