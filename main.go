package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/cloudflare/cloudflare-go"
)

const (
	// version must be updated when changes affecting cloudflare is made.
	// This is to protect against undoing a fix or a feature applied to
	// cfzone using an older version of cfzone.
	version = 2019121201
)

var (
	// These can be overridden for testing.
	exit   = os.Exit
	stdout = io.Writer(os.Stdout)
	stdin  = io.Reader(os.Stdin)
	stderr = io.Writer(os.Stderr)

	// yes can be set to true to disable the confirmation dialog and sync
	// without asking the user. Will be set to true by the "-yes" flag.
	yes          = false
	leaveUnknown = false
	ignoreSpf    = false
	ignoreSrv    = false
	useToken     = false
	origin       = ""
	zoneAutoTTL  = 0
	zoneCacheTTL = 1
)

var (
	apiKey   = os.Getenv("CF_API_KEY")
	apiEmail = os.Getenv("CF_API_EMAIL")
	apiToken = os.Getenv("CF_API_TOKEN")
)

// parseArguments tries to pass the arguments in args.
// It will return the first ńon-flag argument, and any error encountered
func parseArguments(args []string) (string, error) {
	printVersion := false

	// We do our own flagset to be able to test arguments.
	flagset := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flagset.Usage = func() {
		fmt.Fprintf(flagset.Output(), "Usage of %s [flags] /path/to/zone/file:\n", os.Args[0])
		flagset.PrintDefaults()
	}
	flagset.SetOutput(stderr)
	flagset.BoolVar(&yes, "yes", false, "Don't ask before syncing")
	flagset.BoolVar(&leaveUnknown, "leaveunknown", false, "Don't delete unknown records")
	flagset.BoolVar(&ignoreSpf, "ignorespf", false, "Ignore SPF RR type (Not supported by this tool; use TXT for SPF records)")
	flagset.BoolVar(&ignoreSrv, "ignoresrv", false, "Ignore SRV RR type (Not supported by this tool)")
	flagset.StringVar(&origin, "origin", "", "Specify origin to resolve '@' at the top level")
	flagset.IntVar(&zoneAutoTTL, "autottl", 0, "Specify TTL to interpret as Cloudflare automatic")
	flagset.IntVar(&zoneCacheTTL, "cachettl", 1, "Specify TTL to interpret as Cloudflare caching")
	flagset.BoolVar(&printVersion, "version", false, "Print version")

	err := flagset.Parse(args[1:])

	if printVersion {
		fmt.Printf("%d\n", version)
		exit(0)
	}

	if err == nil && flagset.NArg() < 1 {
		err = errors.New("Zone file must be specified")
		fmt.Fprintln(flagset.Output(), err)
		flagset.Usage()
	}

	return flagset.Arg(0), err
}

func main() {
	path, err := parseArguments(os.Args)
	if err != nil {
		os.Exit(1)
	}

	// If a global key is provided, use it
	// Otherwise, check for a (scoped) token
	if apiKey == "" || apiEmail == "" {
		if apiToken == "" {
			fmt.Fprintf(stderr, "Please set CF_API_KEY and CF_API_EMAIL environment variables\n")
			exit(1)
		}
		useToken = true
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "Error opening '%s': %s\n", path, err.Error())
		exit(1)
	}

	hasher := sha256.New()
	_, err = io.Copy(hasher, f)
	if err != nil {
		fmt.Fprintf(stderr, "Error reading '%s': %s\n", "path", err.Error())
		exit(1)
	}

	_, err = f.Seek(0, 0)
	if err != nil {
		fmt.Fprintf(stderr, "Error seeking '%s': %s\n", path, err.Error())
		exit(1)
	}

	zoneName, fileRecords, err := parseZoneWithOriginAndTTLs(f, origin, zoneAutoTTL, zoneCacheTTL)
	if err != nil {
		fmt.Fprintf(stderr, "Error reading '%s': %s\n", path, err.Error())
		exit(1)
	}

	var api *cloudflare.API
	if useToken {
		api, err = cloudflare.NewWithAPIToken(apiToken)
	} else {
		api, err = cloudflare.New(apiKey, apiEmail)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error contacting Cloudflare: %s\n", err.Error())
		exit(1)
	}

	id, err := api.ZoneIDByName(zoneName)
	if err != nil {
		fmt.Fprintf(stderr, "Can't get zone ID for '%s': %s\n", zoneName, err.Error())
		exit(1)
	}

	allRecords, err := api.DNSRecords(context.Background(), id, cloudflare.DNSRecord{})
	if err != nil {
		fmt.Fprintf(stderr, "Can't get zone records for '%s': %s\n", id, err.Error())
		exit(1)
	}
	var records = make([]cloudflare.DNSRecord, 0, len(allRecords))
	for _, record := range allRecords {
		if record.Type == "SRV" && ignoreSrv {
			continue
		}
		if record.Type == "SPF" && ignoreSpf {
			continue
		}
		records = append(records, record)
	}
	existingRecords := recordCollection(records)

	versionRecord := cloudflare.DNSRecord{
		Name:    "cfzone-version." + zoneName,
		Content: strconv.Itoa(version),
		Type:    "TXT",
		TTL:     600,
	}

	n, versionRecordFound := existingRecords.Find(versionRecord, Updatable)
	if versionRecordFound != nil {
		deployedVersion, _ := strconv.Atoi(versionRecordFound.Content)

		// Check if we risk "downgrading" the cloudflare setup.
		if deployedVersion > version {
			fmt.Fprintf(stdout,
				"Deployed version (%d) is newer than current version (%d). Continue (y/N)? ",
				deployedVersion,
				version)

			if !yesNo(stdin) {
				fmt.Fprintf(stdout, "Aborting...\n")
				exit(0)
			}
		}

		existingRecords.Remove(n)
	}

	// Find records only present at cloudflare - and records only present in
	// the file zone. This will be the basis for the add/delete collections.
	addCandidates := fileRecords.Difference(existingRecords, FullMatch)
	deleteCandidates := existingRecords.Difference(fileRecords, FullMatch)

	// If we find the intersection between file and existing, we should have
	// a list of records to update. We use only Updatable here, because that
	// will give us a collection of records that makes sense to update.
	updates := deleteCandidates.Intersect(addCandidates, Updatable)

	// The records to be updated can be removed from the add and delete
	// collections.
	adds := addCandidates.Difference(updates, Updatable)
	deletes := deleteCandidates.Difference(updates, Updatable)

	if len(deletes) > 0 && leaveUnknown {
		fmt.Fprintf(stdout, "%d unknown records left untouched\n", len(deletes))
		deletes = deletes[:0]
	}

	numChanges := len(updates) + len(adds) + len(deletes)

	if numChanges > 0 && !yes {
		if len(deletes) > 0 {
			fmt.Fprintf(stdout, "Records to delete:\n")
			deletes.Fprint(stdout)
			fmt.Printf("\n")
		}

		if len(adds) > 0 {
			fmt.Fprintf(stdout, "Records to add:\n")
			adds.Fprint(stdout)
			fmt.Printf("\n")
		}

		if len(updates) > 0 {
			fmt.Fprintf(stdout, "Records to update:\n")
			updates.Fprint(stdout)
			fmt.Printf("\n")
		}

		fmt.Fprintf(stdout, "Summary:\n")
		fmt.Fprintf(stdout, "SHA256 zone checksum: %x\n", hasher.Sum(nil))
		fmt.Fprintf(stdout, "Records to delete: %d\n", len(deletes))
		fmt.Fprintf(stdout, "Records to add: %d\n", len(adds))
		fmt.Fprintf(stdout, "Records to update: %d\n", len(updates))
		fmt.Fprintf(stdout, "Unchanged records: %d\n", len(records)-len(deleteCandidates))

		fmt.Fprintf(stdout, "%d change(s). Continue (y/N)? ", numChanges)

		if !yesNo(stdin) {
			fmt.Fprintf(stdout, "Aborting...\n")
			exit(0)
		}
	}

	// We sneak this in after informing the user about updates to avoid
	// polluting the diff and confusing the user.
	if versionRecordFound != nil {
		if versionRecordFound.Content != versionRecord.Content {
			versionRecordFound.Content = versionRecord.Content

			updates = append(updates, *versionRecordFound)
		}
	} else {
		adds = append(addCandidates, versionRecord)
	}

	for _, r := range deletes {
		err = api.DeleteDNSRecord(context.Background(), id, r.ID)
		if err != nil {
			fmt.Fprintf(stderr, "Failed to delete record %+v: %s\n", r, err.Error())
			exit(1)
		}
	}

	for _, r := range adds {
		_, err = api.CreateDNSRecord(context.Background(), id, r)
		if err != nil {
			fmt.Fprintf(stderr, "Failed to add record %+v: %s\n", r, err.Error())
			exit(1)
		}
	}

	for _, r := range updates {
		err = api.UpdateDNSRecord(context.Background(), id, r.ID, r)
		if err != nil {
			fmt.Fprintf(stderr, "Failed to update record %+v: %s\n", r, err.Error())
			exit(1)
		}
	}
}

// yesNo will return true if the user entered Y or y + enter. False in all
// other cases.
func yesNo(r io.Reader) bool {
	line, _, _ := bufio.NewReader(r).ReadLine()

	return strings.ToLower(string(line)) == "y"
}
