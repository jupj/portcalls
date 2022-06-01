package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

func main() {
	err := run()
	if err != nil {
		panic(err)
	}
}

func run() error {
	// Read flags
	var (
		portCode string
		cacheDir = getCacheDir()
		cmd      = filepath.Base(os.Args[0])
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "%s prints Finnish port call info from Fintraffic\n", cmd)
		fmt.Fprintf(flag.CommandLine.Output(), "USAGE:    %s -p FIKOK\n", cmd)
		fmt.Fprintln(flag.CommandLine.Output())
		flag.PrintDefaults()
	}
	flag.StringVar(&portCode, "p", portCode, "Port code")
	flag.StringVar(&cacheDir, "cache", cacheDir, "Cache dir")
	flag.Parse()

	if portCode == "" {
		fmt.Fprintln(flag.CommandLine.Output(), "Please specify port code")
		fmt.Fprintln(flag.CommandLine.Output())
		flag.Usage()
		os.Exit(1)
	}

	// Create cache dir if not exists
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	if err := os.Chdir(cacheDir); err != nil {
		return err
	}

	var pc portCalls
	err := readfile(portCallsFile, &pc)
	if errors.Is(err, os.ErrNotExist) || (err == nil && pc.PortCallsUpdated.Before(time.Now().Add(-time.Hour))) {
		if err := download(portCallsFile, portCallsURL); err != nil {
			return err
		}

		err = readfile(portCallsFile, &pc)
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 4, ' ', 0)
	fmt.Fprintf(tw, "Last update: %s\n", pc.PortCallsUpdated.Local().Format("2006-01-02 15:04"))

	events, err := portEvents(&pc, portCode)
	if err != nil {
		return err
	}
	for _, e := range events {
		// Don't show events that happened more than one day ago
		if e.timestamp().Before(time.Now().AddDate(0, 0, -1)) {
			continue
		}

		vd, err := getVesselDetails(e.mmsi, e.imoLloyds, e.vessel)
		if err != nil {
			log.Printf("No details for %s: %v\n", e.vessel, err)
			vd = &vesselDetails{}
		}

		if e.actual.IsZero() {
			// Estimated time
			fmt.Fprintf(tw, "%s (E)", e.estimate.Local().Format("2006-01-02 15:04"))
		} else {
			// Actual time
			fmt.Fprintf(tw, "%s (A)", e.actual.Local().Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(tw, "\t%s", e.event)
		fmt.Fprintf(tw, "\t%s", e.portArea)
		fmt.Fprintf(tw, "\t%s %s", e.vessel, vd.VesselRegistration.Nationality)
		fmt.Fprintf(tw, "\t%.0fm / %.0fm", math.Round(vd.VesselDimensions.OverallLength), math.Round(vd.VesselDimensions.Breadth))
		fmt.Fprintf(tw, "\t%s", vd.VesselConstruction.VesselTypeName)
		fmt.Fprintln(tw)
	}

	tw.Flush()
	_, err = io.Copy(os.Stdout, &buf)
	return err
}

// *** Fintraffic data models - https://meri.digitraffic.fi/swagger/ ***

const (
	portCallsFile = "portcalls.json"
	portCallsURL  = "https://meri.digitraffic.fi/api/v1/port-calls"
)

// portCalls is returned from portCallsURL
type portCalls struct {
	PortCallsUpdated time.Time `json:"portCallsUpdated"`
	PortCalls        []struct {
		PortToVisit    string `json:"portToVisit"`
		PrevPort       string `json:"prevPort"`
		NextPort       string `json:"nextPort"`
		VesselName     string `json:"vesselName"`
		MMSI           int    `json:"mmsi"`
		IMOLloyds      int    `json:"imoLloyds"`
		IMOInformation []struct {
			ImoGeneralDeclaration string `json:"imoGeneralDeclaration"`
			NumberOfCrew          int    `json:"numberOfCrew"`
			NumberOfPassangers    int    `json:"numberOfPassangers"`
		} `json:"imoInformation"`
		PortAreaDetails []struct {
			PortAreaName string    `json:"portAreaName"`
			ATA          time.Time `json:"ata"`
			ATD          time.Time `json:"atd"`
			ETA          time.Time `json:"eta"`
			ETD          time.Time `json:"etd"`
		} `json:"PortAreaDetails"`
	} `json:"portCalls"`
}

const (
	vesselDetailsFile = "vessel-details-%s-%s.json"
	vesselDetailsURL  = "https://meri.digitraffic.fi/api/v1/metadata/vessel-details?%s=%s"
)

// vesselDetails is returned (as a json array) from vesselDetailsURL
type vesselDetails struct {
	MMSI               int       `json:"mmsi"`
	Name               string    `json:"name"`
	UpdateTimestamp    time.Time `json:"updateTimestamp"`
	VesselConstruction struct {
		VesselTypeCode int    `json:"vesselTypeCode"`
		VesselTypeName string `json:"vesselTypeName"`
	} `json:"vesselConstruction"`
	VesselDimensions struct {
		Length        float64 `json:"length"`
		OverallLength float64 `json:"overallLength"`
		Height        float64 `json:"height"`
		Breadth       float64 `json:"breadth"`
		Draught       float64 `json:"draught"`
	} `json:"vesselDimensions"`
	VesselRegistration struct {
		Nationality    string `json:"nationality"`
		PortOfRegistry string `json:"portOfRegistry"`
	} `json:"vesselRegistration"`
}

// getVesselDetails retrieves the details for a vessel (per mmsi) from Fintraffic
func getVesselDetails(mmsi, imoLloyds int, name string) (*vesselDetails, error) {
	var key, value string
	switch {
	case mmsi > 0:
		key = "mmsi"
		value = strconv.Itoa(mmsi)
	case imoLloyds > 0:
		key = "imo"
		value = strconv.Itoa(imoLloyds)
	case name != "":
		key = "vesselName"
		value = url.QueryEscape(name)
	default:
		return nil, errors.New("no mmsi, imoLloyds or name specified")
	}
	filename := fmt.Sprintf(vesselDetailsFile, key, value)
	url := fmt.Sprintf(vesselDetailsURL, key, value)

	var vds []vesselDetails
	err := readfile(filename, &vds)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(vds) == 0) {
		if err := download(filename, url); err != nil {
			return nil, err
		}

		err = readfile(filename, &vds)
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}

	if len(vds) != 1 {
		return nil, fmt.Errorf("getVesselDetails: expected 1 item, got %d", len(vds))
	}

	return &vds[0], nil
}

// *** Utils ***

// readfile decodes a json file into v
func readfile(filename string, v any) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	err = json.NewDecoder(f).Decode(v)
	if err != nil {
		return err
	}

	return nil
}

// download gets the url and saves it to filename
func download(filename, url string) error {
	log.Printf("Downloading %s into %s\n", url, filename)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// portEvent represent a port arrival/departure
type portEvent struct {
	event     string
	vessel    string
	mmsi      int
	imoLloyds int
	portArea  string
	estimate  time.Time
	actual    time.Time
}

func (p *portEvent) timestamp() time.Time {
	if p.actual.IsZero() {
		return p.estimate
	}
	return p.actual
}

// portEvents gets all events for a specific port
func portEvents(pc *portCalls, port string) ([]portEvent, error) {
	var events []portEvent

	for _, call := range pc.PortCalls {
		if call.PortToVisit != port {
			continue
		}
		for _, area := range call.PortAreaDetails {
			events = append(events, portEvent{
				event:     "Arrival",
				vessel:    call.VesselName,
				mmsi:      call.MMSI,
				imoLloyds: call.IMOLloyds,
				portArea:  area.PortAreaName,
				estimate:  area.ETA.UTC(),
				actual:    area.ATA.UTC(),
			})

			events = append(events, portEvent{
				event:     "Departure",
				vessel:    call.VesselName,
				mmsi:      call.MMSI,
				imoLloyds: call.IMOLloyds,
				portArea:  area.PortAreaName,
				estimate:  area.ETD.UTC(),
				actual:    area.ATD.UTC(),
			})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].timestamp().Before(events[j].timestamp()) })

	return events, nil
}

func getCacheDir() string {
	appname := filepath.Base(os.Args[0])

	// Use the XDG cache dir in first place
	if os.Getenv("XDG_CACHE_HOME") != "" {
		return filepath.Join(os.Getenv("XDG_CACHE_HOME"), appname)
	}

	if os.Getenv("HOME") != "" {
		return filepath.Join(os.Getenv("HOME"), ".cache", appname)
	}

	// Try typical Windows environment variables
	if os.Getenv("APPDATA") != "" {
		return filepath.Join(os.Getenv("APPDATA"), appname)
	}

	if os.Getenv("HOMEPATH") != "" {
		return filepath.Join(os.Getenv("HOMEPATH"), ".cache", appname)
	}

	// No env vars found => return current working directory
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("fatal error: %v", err)
	}
	return wd
}
