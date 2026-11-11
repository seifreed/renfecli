package main

import (
	"fmt"
	"os"

	"github.com/seifreed/renfecli/internal/client"
)

// cmdCalendar surfaces the price strip Renfe ships alongside every search: the
// cheapest fare per day for the days around the one you asked for. It is the
// same one search prints a single line of, and it costs no extra request — the
// search response already carries it.
func cmdCalendar(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("calendar")
	sf := addSearchFlags(fs)
	onlyBest := fs.Bool("cheapest", false, "show only the cheapest day")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	_, _, res, err := runSearch("calendar", sf, fs.Args(), correlationIDs...)
	if err != nil {
		return err
	}
	days := res.Calendar
	if len(days) == 0 {
		return fmt.Errorf("renfe returned no price calendar for %s → %s", res.From, res.To)
	}
	if *onlyBest {
		// cheapestDay reports the zero PriceDay when the whole strip is sold
		// out. Printing that emits a day with an empty date and a price of
		// zero — a day that does not exist — so say what actually happened.
		best := cheapestDay(days)
		if best.Date == "" {
			return fmt.Errorf("no day in Renfe's price strip for %s → %s has anything on sale",
				res.From, res.To)
		}
		days = []client.PriceDay{best}
	}
	if emitted, err := emitStructured(cf, days); emitted {
		return err
	}
	return printCalendar(os.Stdout, res, days, cheapestDay(res.Calendar))
}

// cheapestDay picks the lowest priced available day; the zero PriceDay when
// every day in the strip is sold out.
func cheapestDay(days []client.PriceDay) client.PriceDay {
	var best client.PriceDay
	for _, d := range days {
		if !d.Available || d.MinPrice <= 0 {
			continue
		}
		if best.Date == "" || d.MinPrice < best.MinPrice {
			best = d
		}
	}
	return best
}
