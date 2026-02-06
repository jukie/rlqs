package config

import (
	"context"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// Watcher watches a config file for changes and sends new configs on a channel.
type Watcher struct {
	path     string
	logger   *zap.Logger
	debounce time.Duration
}

// NewWatcher creates a Watcher for the given config file path.
func NewWatcher(path string, logger *zap.Logger) *Watcher {
	return &Watcher{
		path:     path,
		logger:   logger,
		debounce: 500 * time.Millisecond,
	}
}

// Watch starts watching the config file and sends validated configs on the
// returned channel. The channel is closed when ctx is cancelled. Editors
// often create multiple filesystem events for a single save, so events are
// debounced.
func (w *Watcher) Watch(ctx context.Context) (<-chan *Config, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := fsw.Add(w.path); err != nil {
		fsw.Close()
		return nil, err
	}

	ch := make(chan *Config, 1)

	go func() {
		defer fsw.Close()
		defer close(ch)

		var timer *time.Timer
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return

			case event, ok := <-fsw.Events:
				if !ok {
					return
				}
				if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
					continue
				}

				// Debounce: reset timer on each write event.
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(w.debounce, func() {
					w.reload(ctx, ch, fsw)
				})

			case err, ok := <-fsw.Errors:
				if !ok {
					return
				}
				w.logger.Error("config watcher error", zap.Error(err))
			}
		}
	}()

	w.logger.Info("config watcher started", zap.String("path", w.path))
	return ch, nil
}

// reload loads and validates the config, then sends it on the channel.
func (w *Watcher) reload(ctx context.Context, ch chan<- *Config, fsw *fsnotify.Watcher) {
	cfg, err := Load(w.path)
	if err != nil {
		w.logger.Error("config reload failed: invalid config, keeping current",
			zap.String("path", w.path),
			zap.Error(err))
		return
	}

	// Re-add the watch in case the file was replaced (editors do
	// rename+create instead of in-place write).
	_ = fsw.Remove(w.path)
	if err := fsw.Add(w.path); err != nil {
		w.logger.Error("config watcher: failed to re-watch file",
			zap.String("path", w.path),
			zap.Error(err))
	}

	select {
	case ch <- cfg:
		w.logger.Info("config reloaded successfully", zap.String("path", w.path))
	case <-ctx.Done():
	}
}
