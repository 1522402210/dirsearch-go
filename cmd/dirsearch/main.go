// This software is a Go implementation of dirsearch by Mauro Soria
// (maurosoria at gmail dot com) written by Simone Margaritelli
// (evilsocket at gmail dot com).
// further development by @eur0pa and @jimen0

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eur0pa/dirsearch-go"
	"github.com/eur0pa/dirsearch-go/brutemachine"
	"github.com/fatih/color"
	"github.com/jpillora/go-tld"
)

var (
	base      = flag.String("u", "", "URL to enumerate, use {} to replace keyword")
	wordlist  = flag.String("w", "dict.txt", "Wordlist file")
	method    = flag.String("M", "GET", "Request method (HEAD / GET)")
	ext       = flag.String("e", "", "Extension to add to requests ('.ext,.ext')")
	cookie    = flag.String("c", "", "Cookies (format: name=value;name=value)")
	skipCode  = flag.String("x", "", "Status codes to exclude (comma sep)")
	skipSize  = flag.String("s", "", "Skip sizes (comma sep)")
	useragent = flag.String("U", "random", "Custom user agent")
	headers   = flag.String("H", "", "Add custom header (name:value;name=value)")
	maxerrors = flag.Uint64("E", 10, "Max. errors before exiting")
	delay     = flag.Int64("d", 0, "Delay between requests (milliseconds)")
	sizeMin   = flag.Int64("sm", -1, "Skip size (min value)")
	sizeMax   = flag.Int64("sM", -1, "Skip size (max value)")
	threads   = flag.Int("t", 10, "Number of concurrent goroutines")
	timeout   = flag.Int("T", 10, "Timeout before killing the request")
	only200   = flag.Bool("2", false, "Only display responses with 200 status code")
	follow    = flag.Bool("f", false, "Follow redirects")
	waf       = flag.Bool("waf", false, "Inject 'WAF bypass' headers")
	verbose   = flag.Bool("v", false, "Verbose: print all results (except 404)")
	debug     = flag.Bool("vv", false, "Debug: dump bodies")

	// declare this here
	client    *http.Client
	transport *http.Transport

	// replace keywords map
	replace = make(map[string]string)

	g = color.New(color.FgGreen)
	y = color.New(color.FgYellow)
	r = color.New(color.FgRed)
	b = color.New(color.FgBlue)
	n = color.New(color.FgMagenta)

	m          *brutemachine.Machine
	wfuzz      bool
	normalized string
	extensions []string
	errors     = uint64(0)
	skipCodes  = make(map[int]struct{})
	skipSizes  = make(map[int64]struct{})
	test404    = []string{
		"th1s-1s-4-r4nd0m-f1l3",
		"th1s-1s-4-r4nd0m-f0ld3r/",
		".htpasswdAncheNo",
		"adminFalsucci",
	}
)

// isAlive checks if host is alive before going all the trouble
func isAlive(url string) bool {
	res, err := client.Get(*base)
	if err != nil {
		r.Fprintln(os.Stderr, "could not connect to", *base, err)
		return false
	}

	defer res.Body.Close()

	// no :(
	io.Copy(ioutil.Discard, res.Body)

	return true
}

// returns the content-length or the size of the body
// note: content-length can lie
func contentLength(res *http.Response) (int64, error) {
	var err error
	size := res.ContentLength

	if size <= 0 {
		cl := res.Header.Get("Content-Length")
		size, err = strconv.ParseInt(cl, 10, 64)
		if size <= 0 || err != nil {
			b, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return 0, err
			}
			size = int64(len(b))
		}
	}

	// no :( don't touch this
	io.Copy(ioutil.Discard, res.Body)
	return size, nil
}

// check404 makes a bogus request to calibrate the 404 engine
func check404(url string) (int, int64, error) {
	res, err := client.Get(url)
	if err != nil {
		return 0, 0, err
	}

	defer res.Body.Close()

	size, err := contentLength(res)
	if err != nil {
		return 0, 0, err
	}

	return res.StatusCode, size, nil
}

// do sends HTTP requests to the page.
func do(page, ext string) brutemachine.Printer {
	// base url + word
	url := *base + page

	// replace à la wfuzz:
	// https://domain.tld/{}/{}.ext -> https://domain.tld/word/word.ext
	if wfuzz {
		url = strings.Replace(*base, "{}", page, -1)
	}

	// replace keywords, after the initial {}
	// {SUB} : target's subdomains
	// {HOST}: target's root domain name
	// {TLD} : target's top-level domain or public suffix
	// {YEAR}: current year as YYYY
	r := strings.NewReplacer("{SUB}", replace["sub"],
		"{HOST}", replace["host"],
		"{TLD}", replace["tld"],
		"{YEAR}", replace["year"])
	url = r.Replace(url)

	// add .ext to every request, or replace where needed
	// 06/08: %EXT% removed for the time being, bug a rotta de collo
	if ext != "" {
		url = url + ext
	}

	// print progress
	if m.Stats.Execs%100 == 0 {
		printStatus()
	}

	// build request
	req, err := http.NewRequest(*method, url, nil)
	if err != nil {
		return nil
	}

	// some servers have issues with */*, some others will serve
	// different content
	ua := dirsearch.GetRandomUserAgent()
	if *useragent != "random" && *useragent != "" {
		ua = *useragent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")

	// add cookies
	if *cookie != "" {
		req.Header.Set("Cookie", *cookie)
	}

	// add custom headers
	if *headers != "" {
		for _, hh := range strings.Split(*headers, ";") {
			h := strings.Split(hh, ":")
			if h[0] != "" && h[1] != "" {
				k, v := h[0], h[1]
				req.Header.Set(k, v)
			}
		}
	}

	// attempt to bypass waf if asked to do so
	if *waf {
		req.Header.Set("Referer", url)
		req.Header.Set("X-Client-IP", "127.0.0.1")
		req.Header.Set("X-Remote-IP", "127.0.0.1")
		req.Header.Set("X-Remote-Addr", "127.0.0.1")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("X-Originating-IP", "127.0.0.1")
	}

	res, err := client.Do(req)
	if err != nil {
		atomic.AddUint64(&errors, 1)
		return &Result{
			url: req.RequestURI,
			err: fmt.Errorf("error: %v", err),
		}
	}

	defer res.Body.Close()

	if *debug {
		r, err := httputil.DumpRequest(req, true)
		if err == nil {
			b, _ := ioutil.ReadAll(res.Body)
			fmt.Println(string(r))
			fmt.Println(string(b))
		}
	}

	_, skip := skipCodes[res.StatusCode]

	// "skip status code" logic:
	//   - if 200 and only 200 -> pass
	//   - if not detected as wildcard, and not only 200 -> pass
	//   - if verbose -> pass
	if ((res.StatusCode == http.StatusOK) && *only200) || (!skip && !*only200) || *verbose {
		location := res.Header.Get("Location")

		size, err := contentLength(res)
		if err != nil {
			return &Result{url, res.StatusCode, 0, location, err}
		}

		// skip certain sizes (auto-skip, and user defined)
		_, skip := skipSizes[size]
		if skip && !*verbose {
			return nil
		}

		// skip a range of sizes
		if size >= *sizeMin && size <= *sizeMax && !*verbose {
			return nil
		}

		return &Result{
			url:      url,
			status:   res.StatusCode,
			size:     size,
			location: location,
		}
	}

	// bad jimeno :(
	io.Copy(ioutil.Discard, res.Body)

	return nil
}

// onResult handles each result.
var onResult = func(res brutemachine.Printer) {
	if errors > *maxerrors {
		r.Fprintf(os.Stderr, "\nExceeded %d errors, quitting...\n", *maxerrors)
		os.Exit(1)
	}
	res.Print()
}

// summary prints a short summary.
func summary() {
	codes := make([]int, 0, len(skipCodes))
	for key := range skipCodes {
		if key != 404 {
			codes = append(codes, key)
		}
	}

	sizes := make([]int64, 0, len(skipSizes))
	for key := range skipSizes {
		sizes = append(sizes, key)
	}

	fmt.Fprintf(os.Stderr, "\nSkip codes: %v\n", codes)
	fmt.Fprintf(os.Stderr, "Skip sizes: %v", sizes)
	if *sizeMin > 0 && *sizeMax > 0 {
		fmt.Fprintf(os.Stderr, " + [%d-%d]", *sizeMin, *sizeMax)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "*")
	}
	fmt.Fprintf(os.Stderr, "\nExtensions: %v", extensions)
	if wfuzz {
		fmt.Fprintf(os.Stderr, "*")
	}
	fmt.Fprintf(os.Stderr, "\nWorkers   : [%d / %d ms]\n\n", *threads, *delay)
}

func main() {
	setup()

	// wfuzz style?
	if strings.Contains(*base, "{}") {
		wfuzz = true
		*base = strings.TrimRight(*base, "/")
	}

	// create a list of extensions
	if *ext != "" {
		extensions = append(extensions, strings.Split(*ext, ",")...)
	}

	// create a list of exclusions
	if *skipCode != "" {
		for _, x := range strings.Split(*skipCode, ",") {
			y, err := strconv.Atoi(x)
			if err != nil {
				r.Fprintln(os.Stderr, "could not parse code:", x)
				continue
			}
			if y != http.StatusOK {
				skipCodes[y] = struct{}{}
			}
		}
	}

	// exclude sizes
	if *skipSize != "" {
		for _, x := range strings.Split(*skipSize, ",") {
			y, err := strconv.ParseInt(x, 10, 64)
			if err != nil {
				r.Fprintln(os.Stderr, "could not parse size:", x)
				continue
			}
			skipSizes[y] = struct{}{}
		}
	}

	// set redirects policy
	if !*follow {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	// check if host is alive.
	if !isAlive(strings.Split(*base, "{")[0]) {
		return
	}

	// calibrate the 404 engine using several red herrings:
	// 1. a random request to a php file, an unnamed file, and a folder
	// 2. a request to a sensible, non-existent hidden file
	// 3. a request with "admin" in it
	for _, test := range test404 {
		x, y, err := check404(strings.Split(*base, "{")[0] + test)
		if err != nil {
			return
		}

		// add found codes and sizes to the skip list
		if x != http.StatusOK && x != http.StatusNotFound {
			skipCodes[x] = struct{}{}
		}
		skipSizes[y] = struct{}{}
	}
	// print a short summary.
	summary()

	// add this last so it won't print
	extensions = append(extensions, "")

	// populate the replacement map
	x, err := tld.Parse(*base)
	if err == nil {
		replace["sub"] = x.Subdomain
		replace["host"] = x.Domain
		replace["tld"] = x.TLD
	}
	replace["year"] = time.Now().Format("2006")

	m = brutemachine.New(*threads, *wordlist, extensions, *delay, do, onResult)
	if err := m.Start(); err != nil {
		r.Fprintln(os.Stderr, "could not start bruteforce:", err)
	}
	m.Wait()

	g.Fprintf(os.Stderr, "\nDONE\n")

	printStats()
}

// Do some initialization.
// NOTE: We can't call this in the 'init' function otherwise
// are gonna be mandatory for unit test modules.
func setup() {
	flag.Parse()

	// initialize the client and transport here *after* parsing the options...
	client = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *threads,
			MaxConnsPerHost:     *threads,
			MaxIdleConnsPerHost: *threads,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Timeout: time.Duration(*timeout) * time.Second,
	}

	var err error
	*base, err = dirsearch.NormalizeURL(*base)
	if err != nil {
		fmt.Println(err)
		flag.Usage()
		os.Exit(1)
	}

	// seed RNG
	rand.Seed(time.Now().Unix())

	// if interrupted, print statistics and exit
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		r.Fprintf(os.Stderr, "\nINTERRUPTING...\n")
		printStats()
		os.Exit(0)
	}()
}

// Print some stats
func printStats() {
	m.UpdateStats()

	fmt.Fprintln(os.Stderr, "Requests  :", m.Stats.Execs)
	fmt.Fprintln(os.Stderr, "Errors    :", errors)
	fmt.Fprintln(os.Stderr, "Results   :", m.Stats.Results)
	fmt.Fprintln(os.Stderr, "Time      :", m.Stats.Total)
	fmt.Fprintln(os.Stderr, "Req/s     :", m.Stats.Eps, "\n")
}

// Print status bar
func printStatus() {
	m.UpdateStats()
	fmt.Fprintf(os.Stderr, "Status    : %d / %d (%.0f Req/s)\r", m.Stats.Execs, m.Stats.Inputs, m.Stats.Eps)
}
