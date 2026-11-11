package main

import (
	"fmt"
	"os"

	"github.com/seifreed/renfecli/internal/client"
)

// cmdStops prints a train's intermediate calls. Renfe has no endpoint that
// takes a train number on its own: the itinerary bean matches the journey
// against the list cached in the session, so this searches first and then asks
// about one of the results.
func cmdStops(args []string, correlationIDs ...string) error {
	fs, cf := newCommonFlags("stops")
	sf := addSearchFlags(fs)
	train := fs.String("train", "", "train number to detail (default: the first journey found)")
	at := fs.String("at", "", "pick the journey departing at this time (HH:MM)")
	parseFlags(fs, args)

	cl, _, res, err := runSearch("stops", sf, fs.Args(), correlationIDs...)
	if err != nil {
		return err
	}
	j, err := pickJourney(res.Journeys, *train, *at)
	if err != nil {
		return err
	}
	it, err := cl.Stops(j)
	if err != nil {
		return err
	}
	if emitted, err := emitStructured(cf, it); emitted {
		return err
	}
	return printItinerary(os.Stdout, it)
}

// pickJourney selects which of the day's journeys to detail. Without --train or
// --at the first one is used, which is the earliest departure.
func pickJourney(js []client.Journey, train, at string) (client.Journey, error) {
	if len(js) == 0 {
		return client.Journey{}, fmt.Errorf("no journeys to detail")
	}
	switch {
	case train != "":
		want := client.TrainNumber(train)
		for _, j := range js {
			for _, t := range j.Trains {
				if t == want {
					return j, nil
				}
			}
		}
		return client.Journey{}, fmt.Errorf("no train %s in that day's results — run: renfe search … to list them", train)
	case at != "":
		want, err := normaliseClock(at, "--at")
		if err != nil {
			return client.Journey{}, err
		}
		for _, j := range js {
			if j.Departure == want {
				return j, nil
			}
		}
		return client.Journey{}, fmt.Errorf("no journey departs at %s — run: renfe search … to list them", want)
	}
	return js[0], nil
}
