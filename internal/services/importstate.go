package services

import "encoding/json"

// resumeState mirrors the JSON shape the importer persists in
// ImportSession.Notes (internal/importer's sessionState). ImportService writes
// this blob when it creates a session so pipeline.ResumeSession can reload the
// original import options and drive the run. The field names and json tags MUST
// stay in lock-step with internal/importer/run.go's sessionState; this is the
// documented integration contract between the service layer and the importer's
// resume mechanism.
type resumeState struct {
	Mode            string   `json:"mode"`
	SourceRoot      string   `json:"sourceRoot"`
	DestinationRoot string   `json:"destinationRoot"`
	EventName       string   `json:"eventName"`
	SourceID        string   `json:"sourceId"`
	Reorganize      bool     `json:"reorganize"`
	Concurrency     int      `json:"concurrency"`
	Notes           []string `json:"notes,omitempty"`
}

// encode serializes the resume state for storage in ImportSession.Notes.
func (s resumeState) encode() string {
	raw, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// decodeResumeState parses a session's Notes blob. An empty or unparseable blob
// yields a zero resumeState (and, for empty input, no error).
func decodeResumeState(notes string) (resumeState, error) {
	var st resumeState
	if notes == "" {
		return st, nil
	}
	if err := json.Unmarshal([]byte(notes), &st); err != nil {
		return resumeState{}, err
	}
	return st, nil
}
