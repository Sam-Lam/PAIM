package services

import "context"

// Dialoger presents native file dialogs on the Go side. It is implemented in
// main.go over the Wails DialogManager and injected into services that need a
// folder picker (Import, Cleanup) or a save-file dialog (Log export). Both
// methods return an empty string (and nil error) when the user cancels.
type Dialoger interface {
	// PickFolder opens a directory chooser titled title and returns the selected
	// absolute path, or "" if cancelled.
	PickFolder(ctx context.Context, title string) (string, error)
	// SaveFile opens a save dialog seeded with defaultName and returns the chosen
	// absolute path, or "" if cancelled.
	SaveFile(ctx context.Context, defaultName string) (string, error)
}
