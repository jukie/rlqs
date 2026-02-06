package config

import "reflect"

// ReloadScope categorizes config sections by their hot-reload capability.
type ReloadScope string

const (
	// ScopeHotReload means the section can be applied without a restart.
	ScopeHotReload ReloadScope = "hot-reload"
	// ScopeRequiresRestart means the section requires a server restart.
	ScopeRequiresRestart ReloadScope = "requires-restart"
)

// Change describes a changed config section.
type Change struct {
	Section string      `json:"section"`
	Scope   ReloadScope `json:"scope"`
}

// Diff compares two configs and returns which top-level sections changed,
// along with whether each change can be hot-reloaded or requires a restart.
func Diff(old, new *Config) []Change {
	var changes []Change

	if !reflect.DeepEqual(old.Engine, new.Engine) {
		changes = append(changes, Change{
			Section: "engine",
			Scope:   ScopeHotReload,
		})
	}
	if !reflect.DeepEqual(old.Server, new.Server) {
		changes = append(changes, Change{
			Section: "server",
			Scope:   ScopeRequiresRestart,
		})
	}
	if !reflect.DeepEqual(old.Storage, new.Storage) {
		changes = append(changes, Change{
			Section: "storage",
			Scope:   ScopeRequiresRestart,
		})
	}
	if !reflect.DeepEqual(old.Tracing, new.Tracing) {
		changes = append(changes, Change{
			Section: "tracing",
			Scope:   ScopeRequiresRestart,
		})
	}

	return changes
}
