package main

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/seifreed/renfecli/internal/client"
	"github.com/seifreed/renfecli/internal/store"
)

const (
	stationsCache = "stations.json"
	// The catalogue changes when a line opens or a station is renamed — a few
	// times a year. A week-old copy is always fine and saves a 270 KB download
	// on every single search.
	stationsMaxAge = 7 * 24 * time.Hour
)

// stationsFile is the cache envelope: the catalogue plus when it was fetched.
type stationsFile struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Stations  []client.Station `json:"stations"`
}

// loadStations returns the station catalogue, from the cache when it is fresh
// and from renfe.com otherwise. A failed refresh falls back to a stale cache:
// an offline moment should not stop you resolving "madrid".
func loadStations(cl *client.Client, force bool) ([]client.Station, error) {
	started := time.Now()
	var cached stationsFile
	haveCache := store.Load(stationsCache, &cached) == nil && len(cached.Stations) > 0
	// A timestamp in the future — a clock that was wrong when the file was
	// written, or a hand-edited copy — makes time.Since negative, which would
	// otherwise read as "fresh" forever.
	age := time.Since(cached.FetchedAt)
	if haveCache && !force && age >= 0 && age < stationsMaxAge {
		stderrEvent(slog.LevelDebug, "station catalogue cache hit", "component", "store", "station_count", len(cached.Stations), "age_ms", age.Milliseconds())
		return cached.Stations, nil
	}
	if force {
		stderrEvent(slog.LevelDebug, "station catalogue refresh forced", "component", "store")
	}
	fresh, err := cl.Stations()
	if err != nil {
		if haveCache {
			stderrEvent(slog.LevelWarn, "station catalogue refresh failed; using stale cache", "component", "store", "station_count", len(cached.Stations), "age_ms", age.Milliseconds(), "error_type", fmt.Sprintf("%T", err))
			return cached.Stations, nil
		}
		stderrEvent(slog.LevelError, "station catalogue unavailable", "component", "store", "outcome", "error", "duration_ms", time.Since(started).Milliseconds())
		return nil, err
	}
	if err := store.Save(stationsCache, stationsFile{FetchedAt: time.Now(), Stations: fresh}); err != nil {
		stderrEvent(slog.LevelWarn, "station catalogue cache write failed", "component", "store", "error_type", fmt.Sprintf("%T", err))
	} else {
		stderrEvent(slog.LevelInfo, "station catalogue refreshed", "component", "store", "station_count", len(fresh), "duration_ms", time.Since(started).Milliseconds())
	}
	return fresh, nil
}

func cmdStations(args []string) error {
	fs, cf := newCommonFlags("stations")
	update := fs.Bool("update", false, "re-download the catalogue, ignoring the cache")
	limit := fs.Int("limit", 20, "cap the number of matches shown (0 = no cap)")
	parseFlags(fs, args)

	if err := nonNegative("--limit", *limit); err != nil {
		return err
	}
	cl := newClient(loadConfig())
	all, err := loadStations(cl, *update)
	if err != nil {
		return err
	}
	query := joinArgs(fs.Args())
	hits := all
	if query != "" {
		hits = client.FindStations(all, query)
	}
	if *limit > 0 && len(hits) > *limit {
		hits = hits[:*limit]
	}
	if emitted, err := emitStructured(cf, hits); emitted {
		return err
	}
	if len(hits) == 0 {
		// Exit non-zero on no match, as grep does, so a caller can branch on it;
		// --json already says the same with an empty array.
		return fmt.Errorf("no station matches %q", query)
	}
	for _, s := range hits {
		fmt.Printf("%-6s  %s\n", s.Code, s.Name)
	}
	return nil
}

// joinArgs folds a multi-word station name back into one query ("san sebastian
// donostia" arrives as three arguments).
func joinArgs(args []string) string { return strings.TrimSpace(strings.Join(args, " ")) }
