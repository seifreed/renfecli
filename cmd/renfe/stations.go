package main

import (
	"fmt"
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
	var cached stationsFile
	haveCache := store.Load(stationsCache, &cached) == nil && len(cached.Stations) > 0
	// A timestamp in the future — a clock that was wrong when the file was
	// written, or a hand-edited copy — makes time.Since negative, which would
	// otherwise read as "fresh" forever.
	age := time.Since(cached.FetchedAt)
	if haveCache && !force && age >= 0 && age < stationsMaxAge {
		return cached.Stations, nil
	}
	fresh, err := cl.Stations()
	if err != nil {
		if haveCache {
			stderrWarnf("station catalogue refresh failed (%v) — using the cached copy from %s",
				err, cached.FetchedAt.Format(time.DateOnly))
			return cached.Stations, nil
		}
		return nil, err
	}
	if err := store.Save(stationsCache, stationsFile{FetchedAt: time.Now(), Stations: fresh}); err != nil {
		stderrWarnf("could not cache the station catalogue: %v", err)
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
