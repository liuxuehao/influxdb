package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	platform "github.com/influxdata/influxdb/v2"
	ihttp "github.com/influxdata/influxdb/v2/http"
	"github.com/influxdata/influxdb/v2/kit/signals"
	"github.com/influxdata/influxdb/v2/models"
	"github.com/influxdata/influxdb/v2/pkg/csv2lp"
	"github.com/influxdata/influxdb/v2/write"
	"github.com/spf13/cobra"
)

const (
	inputFormatCsv          = "csv"
	inputFormatLineProtocol = "lp"
)

type writeFlagsType struct {
	org                        organization
	BucketID                   string
	Bucket                     string
	Precision                  string
	Format                     string
	Headers                    []string
	Files                      []string
	URLs                       []string
	Debug                      bool
	SkipRowOnError             bool
	SkipHeader                 int
	IgnoreDataTypeInColumnName bool
	Encoding                   string
	ErrorsFile                 string
}

var writeFlags writeFlagsType

func cmdWrite(f *globalFlags, opt genericCLIOpts) *cobra.Command {
	cmd := opt.newCmd("write", fluxWriteF, true)
	cmd.Args = cobra.MaximumNArgs(1)
	cmd.Short = "Write points to InfluxDB"
	cmd.Long = `Write data to InfluxDB via stdin, or add an entire file specified with the -f flag`

	f.registerFlags(cmd)
	writeFlags.org.register(cmd, true)
	opts := flagOpts{
		{
			DestP:      &writeFlags.BucketID,
			Flag:       "bucket-id",
			Desc:       "The ID of destination bucket",
			Persistent: true,
		},
		{
			DestP:      &writeFlags.Bucket,
			Flag:       "bucket",
			Short:      'b',
			EnvVar:     "BUCKET_NAME",
			Desc:       "The name of destination bucket",
			Persistent: true,
		},
		{
			DestP:      &writeFlags.Precision,
			Flag:       "precision",
			Short:      'p',
			Default:    "ns",
			Desc:       "Precision of the timestamps of the lines",
			Persistent: true,
		},
	}
	opts.mustRegister(cmd)
	cmd.PersistentFlags().StringVar(&writeFlags.Format, "format", "", "Input format, either lp (Line Protocol) or csv (Comma Separated Values). Defaults to lp unless '.csv' extension")
	cmd.PersistentFlags().StringArrayVar(&writeFlags.Headers, "header", []string{}, "Header prepends lines to input data; Example --header HEADER1 --header HEADER2")
	cmd.PersistentFlags().StringArrayVarP(&writeFlags.Files, "file", "f", []string{}, "The path to the file to import")
	cmd.PersistentFlags().StringArrayVarP(&writeFlags.URLs, "url", "u", []string{}, "The URL to import data from")
	cmd.PersistentFlags().BoolVar(&writeFlags.Debug, "debug", false, "Log CSV columns to stderr before reading data rows")
	cmd.PersistentFlags().BoolVar(&writeFlags.SkipRowOnError, "skipRowOnError", false, "Log CSV data errors to stderr and continue with CSV processing")
	cmd.PersistentFlags().IntVar(&writeFlags.SkipHeader, "skipHeader", 0, "Skip the first <n> rows from input data")
	cmd.Flag("skipHeader").NoOptDefVal = "1" // skipHeader flag value is optional, skip the first header when unspecified
	cmd.PersistentFlags().BoolVar(&writeFlags.IgnoreDataTypeInColumnName, "xIgnoreDataTypeInColumnName", false, "Ignores dataType which could be specified after ':' in column name")
	cmd.PersistentFlags().MarkHidden("xIgnoreDataTypeInColumnName") // should be used only upon explicit advice
	cmd.PersistentFlags().StringVar(&writeFlags.Encoding, "encoding", "UTF-8", "Character encoding of input files or stdin")
	cmd.PersistentFlags().StringVar(&writeFlags.ErrorsFile, "errors-file", "", "The path to the file to write rejected rows to")

	cmdDryRun := opt.newCmd("dryrun", fluxWriteDryrunF, false)
	cmdDryRun.Args = cobra.MaximumNArgs(1)
	cmdDryRun.Short = "Write to stdout instead of InfluxDB"
	cmdDryRun.Long = `Write protocol lines to stdout instead of InfluxDB. Troubleshoot conversion from CSV to line protocol.`
	f.registerFlags(cmdDryRun)
	cmd.AddCommand(cmdDryRun)
	return cmd
}

func (writeFlags *writeFlagsType) dump(args []string) {
	if writeFlags.Debug {
		log.Printf("WriteFlags%+v args:%v", *writeFlags, args)
	}
}

// createLineReader uses writeFlags and cli arguments to create a reader that produces line protocol
func (writeFlags *writeFlagsType) createLineReader(ctx context.Context, cmd *cobra.Command, args []string) (io.Reader, io.Closer, error) {
	files := writeFlags.Files
	if len(args) > 0 && len(args[0]) > 1 && args[0][0] == '@' {
		// backward compatibility: @ in arg denotes a file
		files = append(files, args[0][1:])
		args = args[:0]
	}

	readers := make([]io.Reader, 0, 2*len(writeFlags.Headers)+2*len(files)+2*len(writeFlags.URLs)+1)
	closers := make([]io.Closer, 0, len(files)+len(writeFlags.URLs))

	// validate input format
	if len(writeFlags.Format) > 0 && writeFlags.Format != inputFormatLineProtocol && writeFlags.Format != inputFormatCsv {
		return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("unsupported input format: %s", writeFlags.Format)
	}

	// validate and setup decoding of files/stdin if encoding is supplied
	decode, err := csv2lp.CreateDecoder(writeFlags.Encoding)
	if err != nil {
		return nil, csv2lp.MultiCloser(closers...), err
	}

	// prepend header lines
	if len(writeFlags.Headers) > 0 {
		for _, header := range writeFlags.Headers {
			readers = append(readers, strings.NewReader(header), strings.NewReader("\n"))
		}
		if len(writeFlags.Format) == 0 {
			writeFlags.Format = inputFormatCsv
		}
	}

	// add files
	if len(files) > 0 {
		for _, file := range files {
			f, err := os.Open(file)
			if err != nil {
				return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to open %q: %v", file, err)
			}
			closers = append(closers, f)
			readers = append(readers, decode(f), strings.NewReader("\n"))
			if len(writeFlags.Format) == 0 && strings.HasSuffix(file, ".csv") {
				writeFlags.Format = inputFormatCsv
			}
		}
	}

	// #18349 allow URL data sources, a simple alternative to `curl -f -s http://... | influx write ...`
	if len(writeFlags.URLs) > 0 {
		client := http.DefaultClient
		for _, addr := range writeFlags.URLs {
			u, err := url.Parse(addr)
			if err != nil {
				return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to open %q: %v", addr, err)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
			if err != nil {
				return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to open %q: %v", addr, err)
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to open %q: %v", addr, err)
			}
			closers = append(closers, resp.Body)
			if resp.StatusCode/100 != 2 {
				return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to open %q: response status_code=%d", addr, resp.StatusCode)
			}
			readers = append(readers, decode(resp.Body), strings.NewReader("\n"))
			if len(writeFlags.Format) == 0 &&
				(strings.HasSuffix(u.Path, ".csv") || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv")) {
				writeFlags.Format = inputFormatCsv
			}
		}
	}

	// add stdin or a single argument
	switch {
	case len(args) == 0:
		// use also stdIn if it is a terminal
		if !isCharacterDevice(cmd.InOrStdin()) {
			readers = append(readers, decode(cmd.InOrStdin()))
		}
	case args[0] == "-":
		// "-" also means stdin
		readers = append(readers, decode(cmd.InOrStdin()))
	default:
		readers = append(readers, strings.NewReader(args[0]))
	}

	// skipHeader lines when set
	if writeFlags.SkipHeader != 0 {
		// find the last non-string reader (stdin or file)
		for i := len(readers) - 1; i >= 0; i-- {
			_, stringReader := readers[i].(*strings.Reader)
			if !stringReader { // ignore headers and new lines
				readers[i] = csv2lp.SkipHeaderLinesReader(writeFlags.SkipHeader, readers[i])
				break
			}
		}
	}

	// create writer for errors-file, if supplied
	var errorsFile *csv.Writer
	var rowSkippedListener func(*csv2lp.CsvToLineReader, error, []string)
	if writeFlags.ErrorsFile != "" {
		writer, err := os.Create(writeFlags.ErrorsFile)
		if err != nil {
			return nil, csv2lp.MultiCloser(closers...), fmt.Errorf("failed to create %q: %v", writeFlags.ErrorsFile, err)
		}
		closers = append(closers, writer)
		errorsFile = csv.NewWriter(writer)
		rowSkippedListener = func(source *csv2lp.CsvToLineReader, lineError error, row []string) {
			log.Println(lineError)
			errorsFile.Comma = source.Comma()
			errorsFile.Write([]string{fmt.Sprintf("# error : %v", lineError)})
			if err := errorsFile.Write(row); err != nil {
				log.Printf("Unable to write to error-file: %v\n", err)
			}
			errorsFile.Flush() // flush is required
		}
	}

	// concatenate readers
	r := io.MultiReader(readers...)
	if writeFlags.Format == inputFormatCsv {
		csvReader := csv2lp.CsvToLineProtocol(r)
		csvReader.LogTableColumns(writeFlags.Debug)
		csvReader.SkipRowOnError(writeFlags.SkipRowOnError)
		csvReader.Table.IgnoreDataTypeInColumnName(writeFlags.IgnoreDataTypeInColumnName)
		// change LineNumber to report file/stdin line numbers properly
		csvReader.LineNumber = writeFlags.SkipHeader - len(writeFlags.Headers)
		csvReader.RowSkipped = rowSkippedListener
		r = csvReader
	}
	return r, csv2lp.MultiCloser(closers...), nil
}

func fluxWriteF(cmd *cobra.Command, args []string) error {
	writeFlags.dump(args) // print flags when in Debug mode
	// validate InfluxDB flags
	if err := writeFlags.org.validOrgFlags(&flags); err != nil {
		return err
	}

	if writeFlags.Bucket != "" && writeFlags.BucketID != "" {
		return fmt.Errorf("please specify one of bucket or bucket-id")
	}

	if !models.ValidPrecision(writeFlags.Precision) {
		return fmt.Errorf("invalid precision")
	}

	bs, err := newBucketService()
	if err != nil {
		return err
	}

	var filter platform.BucketFilter
	if writeFlags.BucketID != "" {
		filter.ID, err = platform.IDFromString(writeFlags.BucketID)
		if err != nil {
			return fmt.Errorf("failed to decode bucket-id: %v", err)
		}
	}
	if writeFlags.Bucket != "" {
		filter.Name = &writeFlags.Bucket
	}

	if writeFlags.org.id != "" {
		filter.OrganizationID, err = platform.IDFromString(writeFlags.org.id)
		if err != nil {
			return fmt.Errorf("failed to decode org-id id: %v", err)
		}
	}
	if writeFlags.org.name != "" {
		filter.Org = &writeFlags.org.name
	}

	ctx := signals.WithStandardSignals(context.Background())
	buckets, n, err := bs.FindBuckets(ctx, filter)
	if err != nil {
		return fmt.Errorf("failed to retrieve buckets: %v", err)
	}

	if n == 0 {
		if writeFlags.Bucket != "" {
			return fmt.Errorf("bucket %q was not found", writeFlags.Bucket)
		}

		if writeFlags.BucketID != "" {
			return fmt.Errorf("bucket with id %q does not exist", writeFlags.BucketID)
		}
	}
	bucketID, orgID := buckets[0].ID, buckets[0].OrgID

	// create line reader
	r, closer, err := writeFlags.createLineReader(ctx, cmd, args)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return err
	}

	ac := flags.config()
	// write to InfluxDB
	s := write.Batcher{
		Service: &ihttp.WriteService{
			Addr:               ac.Host,
			Token:              ac.Token,
			Precision:          writeFlags.Precision,
			InsecureSkipVerify: flags.skipVerify,
		},
	}
	if err := s.Write(ctx, orgID, bucketID, r); err != nil && err != context.Canceled {
		return fmt.Errorf("failed to write data: %v", err)
	}

	return nil
}

func fluxWriteDryrunF(cmd *cobra.Command, args []string) error {
	writeFlags.dump(args) // print flags when in Debug mode
	// create line reader
	ctx := signals.WithStandardSignals(context.Background())
	r, closer, err := writeFlags.createLineReader(ctx, cmd, args)
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		return err
	}
	// dry run
	_, err = io.Copy(cmd.OutOrStdout(), r)
	if err != nil {
		return fmt.Errorf("failed: %v", err)
	}
	return nil
}

// IsCharacterDevice returns true if the supplied reader is a character device (a terminal)
func isCharacterDevice(reader io.Reader) bool {
	file, isFile := reader.(*os.File)
	if !isFile {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) == os.ModeCharDevice
}
