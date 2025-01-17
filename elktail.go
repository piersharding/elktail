/*
Copyright (C) 2016 Krešimir Nesek
This software may be modified and distributed under the terms
of the MIT license. See the LICENSE file for details.
*/

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/olivere/elastic/v7"
	configuration "github.com/piersharding/elktail/configuration"
	"github.com/urfave/cli"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/context"
)

//
// Tail is a structure that holds data necessary to perform tailing.
//
type Tail struct {
	client          *elastic.Client                //elastic search client that we'll use to contact EL
	queryDefinition *configuration.QueryDefinition //structure containing query definition and formatting
	indices         []string                       //indices to search through
	lastTimeStamp   string                         //timestamp of the last result
	lastIDs         []displayedEntry               //result IDs that we fetched in the last query, used to avoid duplicates when using tailing query time window
	order           bool                           //search order - true = ascending (may be reversed in case date-after filtering)
	raw             bool                           // Raw output
}

type displayedEntry struct {
	timeStamp string
	id        string
}

func (entry *displayedEntry) isBefore(timeStamp string) bool {
	return entry.timeStamp < timeStamp
}

const dateFormatDMY = "2006-01-02"
const dateFormatFull = "2006-01-02T15:04:05.999Z07:00"
const tailingTimeWindow = 500

// NewTail creates a new Tailer using configuration
func NewTail(configuration *configuration.Configuration) *Tail {
	tail := new(Tail)

	var client *elastic.Client
	var err error
	var url = configuration.SearchTarget.Url
	if !strings.HasPrefix(url, "http") {
		url = "http://" + url
		Trace.Printf("Adding http:// prefix to given url. Url: " + url)
	}

	if !Must(regexp.MatchString(".*:\\d+", url)) && Must(regexp.MatchString("http://[^/]+$", url)) {
		url += ":9200"
		Trace.Printf("No port was specified, adding default port 9200 to given url. Url: " + url)
	}

	//if a tunnel is successfully created, we need to connect to tunnel url (which is localhost on tunnel port)
	if configuration.SearchTarget.TunnelUrl != "" {
		url = configuration.SearchTarget.TunnelUrl
	}

	defaultOptions := []elastic.ClientOptionFunc{
		elastic.SetURL(url),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
		//elastic.SetHealthcheckTimeoutStartup(10 * time.Second),
		//elastic.SetHealthcheckTimeout(2 * time.Second),
	}

	//if configuration.User != "" {
	//	defaultOptions = append(defaultOptions,
	//		elastic.SetBasicAuth(configuration.User, configuration.Password))
	//}

	var cert = configuration.SearchTarget.Cert
	var key = configuration.SearchTarget.Key
	if cert != "" && key != "" {
		cert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			Error.Fatalf("Bad certificate and/or key: %s", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		tlsConfig.BuildNameToCertificate()
		transport := &http.Transport{TLSClientConfig: tlsConfig}
		client := &http.Client{Transport: transport}
		defaultOptions = append(defaultOptions, elastic.SetHttpClient(client))
	}

	if configuration.TraceRequests {
		defaultOptions = append(defaultOptions,
			elastic.SetTraceLog(Trace))
	}

	extraHeaders := map[string]string{}

	for _, header := range configuration.SearchTarget.ExtraHeaders {
		if header != "" {
			tokenized := ExtractHeader(header)
			extraHeaders[tokenized[0]] = tokenized[1]
		}
	}

	version, err := ResolveKibanaVersion(url, extraHeaders)
	if err != nil {
		Info.Println("Cannot resolve kibana version", err)
		version = ""
	}

	httpClient := &http.Client{Transport: KibanaDecorator{r: http.DefaultTransport, kibanaVersion: version, extraHeaders: extraHeaders, configuration: configuration}}
	defaultOptions = append(defaultOptions, elastic.SetHttpClient(httpClient))

	client, err = elastic.NewClient(defaultOptions...)

	if err != nil {
		Error.Fatalf("Could not connect Elasticsearch client to %s: %s.", url, err)
	}
	tail.client = client

	tail.queryDefinition = &configuration.QueryDefinition

	tail.raw = configuration.Raw

	tail.indices = []string{configuration.SearchTarget.IndexPattern}
	//tail.selectIndices(configuration)

	//If we're date filtering on start date, then the sort needs to be ascending
	if configuration.QueryDefinition.AfterDateTime != "" {
		tail.order = true //ascending
	} else {
		tail.order = false //descending
	}
	return tail
}

// Selects appropriate indices in EL based on configuration. This basically means that if query is date filtered,
// then it attempts to select indices in the filtered date range, otherwise it selects the last index.
func (tail *Tail) selectIndices(configuration *configuration.Configuration) {
	result, err := tail.client.CatIndices().Do(context.TODO())
	if err != nil {
		Info.Println("Could not fetch available indices. Using pattern instead.", err)
		tail.indices = []string{configuration.SearchTarget.IndexPattern}
		return
	}
	indices := make([]string, len(result))
	for i, response := range result {
		indices[i] = response.Index
	}

	if configuration.QueryDefinition.IsDateTimeFiltered() {
		startDate := configuration.QueryDefinition.AfterDateTime
		endDate := configuration.QueryDefinition.BeforeDateTime
		if startDate == "" && endDate != "" {
			lastIndex := findLastIndex(indices, configuration.SearchTarget.IndexPattern)
			lastIndexDate := extractYMDDate(lastIndex, ".")
			if lastIndexDate.Before(extractYMDDate(endDate, "-")) {
				startDate = lastIndexDate.Format(dateFormatDMY)
			} else {
				startDate = endDate
			}
		}
		if endDate == "" {
			endDate = time.Now().Format(dateFormatDMY)
		}
		tail.indices = findIndicesForDateRange(indices, configuration.SearchTarget.IndexPattern, startDate, endDate)

	} else {
		index := findLastIndex(indices, configuration.SearchTarget.IndexPattern)
		result := [...]string{index}
		tail.indices = result[:]
	}
	Info.Printf("Using indices: %s", tail.indices)
}

// Start the tailer
func (tail *Tail) Start(follow bool, initialEntries int) {

	result, err := tail.initialSearch(initialEntries)
	if err != nil {
		Error.Fatalln("Error in executing search query.", err)
	}
	tail.processResults(result)
	delay := 500 * time.Millisecond
	for follow {
		time.Sleep(delay)
		if tail.lastTimeStamp != "" {
			Info.Printf("Query: %v\n", tail.buildTimestampFilteredQuery())
			searchRequest := elastic.NewSearchRequest().
				Sort(tail.queryDefinition.TimestampField, false).
				From(0).
				Size(9000). //TODO: needs rewrite this using scrolling, as this implementation may loose entries if there's more than 9K entries per sleep period
				Query(tail.buildTimestampFilteredQuery())

			//we can execute follow up timestamp filtered query only if we fetched at least 1 result in initial query
			multiResult, e := tail.client.MultiSearch().
				Index(tail.indices...).
				Add(searchRequest).
				Do(context.Background())

			if multiResult != nil {
				result = multiResult.Responses[0]
				err = nil
			} else {
				result = nil
				err = e
			}

		} else {
			//if lastTimeStamp is not defined we have to repeat the initial search until we get at least 1 result
			result, err = tail.initialSearch(initialEntries)
			Info.Printf("Query: %s\n", tail.buildTimestampFilteredQuery())
		}
		if err != nil {
			Error.Fatalln("Error in executing search query.", err)
		}
		tail.processResults(result)

		//Dynamic delay calculation for determining delay between search requests
		if result.TotalHits() > 0 && delay > 500*time.Millisecond {
			delay = 500 * time.Millisecond
		} else if delay <= 2000*time.Millisecond {
			delay = delay + 500*time.Millisecond
		}
	}
}

// Initial search needs to be run until we get at least one result
// in order to fetch the timestamp which we will use in subsequent follow searches
func (tail *Tail) initialSearch(initialEntries int) (*elastic.SearchResult, error) {
	searchRequest := elastic.NewSearchRequest().
		Sort(tail.queryDefinition.TimestampField, tail.order).
		Query(tail.buildSearchQuery()).
		From(0).Size(initialEntries)

	result, e := tail.client.MultiSearch().
		Index(tail.indices...).
		Add(searchRequest).
		Do(context.Background())
	if result != nil {
		return result.Responses[0], nil
	} else {
		return nil, e
	}

	// return tail.client.Search().
	// 	Index(tail.indices...).
	// 	Sort(tail.queryDefinition.TimestampField, tail.order).
	// 	Query(tail.buildSearchQuery()).
	// 	From(0).Size(initialEntries).
	// 	Do(context.Background())
}

// Process the results (e.g. prints them out based on configured format)
func (tail *Tail) processResults(searchResult *elastic.SearchResult) {
	Trace.Printf("Fetched page of %d results out of %d total.\n", len(searchResult.Hits.Hits), searchResult.TotalHits())
	hits := searchResult.Hits.Hits

	// We need to track last N entries that had the timestamp newer than cutoff timestamp. This is done to
	// avoid loosing entries that may have arrived to elasticsearch just as we were executing next query.
	// When tailing, we will
	// issue next query which will be filtered so that timestamps are greater or
	// equal to last timestamp minus tailing time window. Since we are tracking IDs of entries form previous query,
	// we can use the IDs to remove the duplicates. https://github.com/knes1/elktail/issues/11

	if tail.order {
		for i := 0; i < len(hits); i++ {
			hit := hits[i]
			entry := tail.processHit(hit)
			timeStamp := entry[tail.queryDefinition.TimestampField].(string)
			if timeStamp != tail.lastTimeStamp {
				tail.lastTimeStamp = timeStamp
			}
			tail.lastIDs = append(tail.lastIDs, displayedEntry{timeStamp: timeStamp, id: hit.Id})
		}

	} else { //when results are in descending order, we need to process them in reverse
		for i := len(hits) - 1; i >= 0; i-- {
			hit := hits[i]
			entry := tail.processHit(hit)
			timeStamp := entry[tail.queryDefinition.TimestampField].(string)
			if timeStamp != tail.lastTimeStamp {
				tail.lastTimeStamp = timeStamp
			}
			tail.lastIDs = append(tail.lastIDs, displayedEntry{timeStamp: timeStamp, id: hit.Id})
		}
	}
	cutoffTime := formatElasticTimeStamp(parseElasticTimeStamp(tail.lastTimeStamp).Add(-tailingTimeWindow * time.Millisecond))
	drainOldEntries(&tail.lastIDs, cutoffTime)
	//fmt.Print("------------------------------------------------\n")
	//Debugging IDs
	//Info.Printf("CutOff time: %s", cutoffTime)
	//Info.Printf("IDs: %v", tail.lastIDs)
}

func parseElasticTimeStamp(elTimeStamp string) time.Time {
	timeStr, _ := time.Parse(dateFormatFull, elTimeStamp)
	return timeStr
}

func formatElasticTimeStamp(timeStamp time.Time) string {
	return timeStamp.Format(dateFormatFull)
}

func drainOldEntries(entries *[]displayedEntry, cutOffTimestamp string) {
	var i int
	for i = 0; i < len(*entries)-1 && (*entries)[i].timeStamp < cutOffTimestamp; i++ {
	}
	*entries = (*entries)[i:]
}

func (tail *Tail) processHit(hit *elastic.SearchHit) map[string]interface{} {
	var entry map[string]interface{}
	err := json.Unmarshal(hit.Source, &entry)
	if err != nil {
		Error.Fatalln("Failed parsing ElasticSearch response.", err)
	}

	if tail.raw {
		fmt.Println(string(hit.Source))
	} else {
		tail.printResult(entry)
	}

	return entry
}

// Regexp for parsing out format fields
var formatRegexp = regexp.MustCompile("%[A-Za-z0-9@_.-]+")

// Print result according to format
func (tail *Tail) printResult(entry map[string]interface{}) {
	fields := formatRegexp.FindAllString(tail.queryDefinition.Format, -1)
	result := tail.queryDefinition.Format
	for _, f := range fields {
		value, _ := EvaluateExpression(entry, f[1:])
		result = strings.Replace(result, f, value, -1)
	}
	fmt.Println(result)
}

func (tail *Tail) buildSearchQuery() elastic.Query {
	var query elastic.Query
	if len(tail.queryDefinition.Terms) > 0 {
		result := strings.Join(tail.queryDefinition.Terms, " ")
		Trace.Printf("Running query string query: %s", result)
		query = elastic.NewQueryStringQuery(result)
	} else {
		Trace.Print("Running query match all query.")
		query = elastic.NewMatchAllQuery()
	}

	if tail.queryDefinition.IsDateTimeFiltered() {
		// we have date filtering turned on, apply filter
		filter := tail.buildDateTimeRangeQuery()
		query = elastic.NewBoolQuery().Filter(query, filter)
	}
	return query
}

//Builds range filter on timestamp field. You should only call this if start or end date times are defined
//in query definition
func (tail *Tail) buildDateTimeRangeQuery() *elastic.RangeQuery {
	filter := elastic.NewRangeQuery(tail.queryDefinition.TimestampField)
	if tail.queryDefinition.AfterDateTime != "" {
		Trace.Printf("Date range query - timestamp after: %s", tail.queryDefinition.AfterDateTime)
		filter = filter.IncludeLower(true).
			From(tail.queryDefinition.AfterDateTime)
	}
	if tail.queryDefinition.BeforeDateTime != "" {
		Trace.Printf("Date range query - timestamp before: %s", tail.queryDefinition.BeforeDateTime)
		filter = filter.IncludeUpper(false).
			To(tail.queryDefinition.BeforeDateTime)
	}
	return filter
}

func (tail *Tail) buildTimestampFilteredQuery() elastic.Query {
	timeStamp := formatElasticTimeStamp(parseElasticTimeStamp(tail.lastTimeStamp).Add(-tailingTimeWindow * time.Millisecond))

	timeStampFilter := elastic.NewRangeQuery(tail.queryDefinition.TimestampField).
		Gte(timeStamp)

	idsToFilter := make([]string, len(tail.lastIDs))
	for i := range tail.lastIDs {
		idsToFilter[i] = tail.lastIDs[i].id
	}

	filter := elastic.NewBoolQuery().Filter(timeStampFilter).MustNot(elastic.NewIdsQuery().Ids(idsToFilter...))
	query := elastic.NewBoolQuery().Filter(tail.buildSearchQuery(), filter)
	return query
}

// Extracts and parses YMD date (year followed by month followed by day) from a given string. YMD values are separated by
// separator character given as argument.
func extractYMDDate(dateStr, separator string) time.Time {
	dateRegexp := regexp.MustCompile(fmt.Sprintf(`(\d{4}%s\d{2}%s\d{2})`, separator, separator))
	match := dateRegexp.FindAllStringSubmatch(dateStr, -1)
	if len(match) == 0 {
		Error.Fatalf("Failed to extract date: %s\n", dateStr)
	}
	result := match[0]
	parsed, err := time.Parse(fmt.Sprintf("2006%s01%s02", separator, separator), result[0])
	if err != nil {
		Error.Fatalf("Failed parsing date: %s", err)
	}
	return parsed
}

func findIndicesForDateRange(indices []string, indexPattern string, startDate string, endDate string) []string {
	start := extractYMDDate(startDate, "-")
	end := extractYMDDate(endDate, "-")
	result := make([]string, 0, len(indices))
	for _, idx := range indices {
		matched, _ := regexp.MatchString(indexPattern, idx)
		if matched {
			idxDate := extractYMDDate(idx, ".")
			if (idxDate.After(start) || idxDate.Equal(start)) && (idxDate.Before(end) || idxDate.Equal(end)) {
				result = append(result, idx)
			}
		}
	}
	return result
}

func findLastIndex(indices []string, indexPattern string) string {
	var lastIdx string
	for _, idx := range indices {
		matched, _ := regexp.MatchString(indexPattern, idx)
		if matched {
			if &lastIdx == nil {
				lastIdx = idx
			} else if idx > lastIdx {
				lastIdx = idx
			}
		}
	}
	return lastIdx
}

func main() {

	config := new(configuration.Configuration)
	app := cli.NewApp()
	app.Name = "elktail"
	app.Usage = "utility for tailing Filebeat logs stored in ElasticSearch"
	app.HideHelp = true
	app.Version = VERSION
	app.ArgsUsage = "[query-string]\n   Options marked with (*) are saved between invocations of the command. Each time you specify an option marked with (*) previously stored settings are erased."
	app.Flags = config.Flags()
	app.Action = func(c *cli.Context) {
		config.SearchTarget.ExtraHeaders = c.StringSlice("header")

		if c.IsSet("help") {
			cli.ShowAppHelp(c)
			os.Exit(0)
		}
		if config.MoreVerbose || config.TraceRequests {
			InitLogging(os.Stderr, os.Stderr, os.Stderr, true)
		} else if config.Verbose {
			InitLogging(ioutil.Discard, os.Stderr, os.Stderr, false)
		} else {
			InitLogging(ioutil.Discard, ioutil.Discard, os.Stderr, false)
		}

		if !configuration.IsConfigRelevantFlagSet(c) {
			loadedConfig, err := configuration.LoadDefault()
			if err != nil {
				Info.Printf("Failed to find or open previous default configuration: %s\n", err)
			} else {
				Info.Printf("Loaded previous config and connecting to host %s.\n", loadedConfig.SearchTarget.Url)
				loadedConfig.CopyConfigRelevantSettingsTo(config)

				if config.MoreVerbose {
					confJs, _ := json.MarshalIndent(loadedConfig, "", "  ")
					Trace.Println("Loaded config:")
					Trace.Println(string(confJs))

					confJs, _ = json.MarshalIndent(loadedConfig, "", "  ")
					Trace.Println("Final (merged) config:")
					Trace.Println(string(confJs))
				}
			}
		}

		if config.User != "" {
			credentials := strings.Split(config.User, ":")
			config.User = credentials[0]
			if len(credentials) == 2 {
				config.Password = credentials[1]
			}
			//fmt.Print("Enter password: ")
			//config.Password = readPasswd()
		}

		//reset TunnelUrl to nothing, we'll point to the tunnel if we actually manage to create it
		config.SearchTarget.TunnelUrl = ""
		if config.SSHTunnelParams != "" {
			//We need to start ssh tunnel and make el client connect to local port at localhost in order to pass
			//traffic through the tunnel
			elurl, err := url.Parse(config.SearchTarget.Url)
			if err != nil {
				Error.Fatalf("Failed to parse hostname/port from given URL: %s\n", config.SearchTarget.Url)
			}
			Trace.Printf("SSHTunnel remote host: %s\n", elurl.Host)

			tunnel := NewSSHTunnelFromHostStrings(config.SSHTunnelParams, elurl.Host)
			//Using the TunnelUrl configuration param, we will signify the client to connect to tunnel
			config.SearchTarget.TunnelUrl = fmt.Sprintf("http://localhost:%d", tunnel.Local.Port)

			Info.Printf("Starting SSH tunnel %d:%s@%s:%d to %s:%d", tunnel.Local.Port, tunnel.Config.User,
				tunnel.Server.Host, tunnel.Server.Port, tunnel.Remote.Host, tunnel.Remote.Port)
			go tunnel.Start()
			Trace.Print("Sleeping for a second until tunnel is established...")
			time.Sleep(1 * time.Second)
		}

		var configToSave *configuration.Configuration

		args := c.Args()

		if config.SaveQuery {
			if args.Present() {
				config.QueryDefinition.Terms = []string{args.First()}
				config.QueryDefinition.Terms = append(config.QueryDefinition.Terms, args.Tail()...)
			} else {
				config.QueryDefinition.Terms = []string{}
			}
			configToSave = config.Copy()
			Trace.Printf("Saving query terms. Total terms: %d\n", len(configToSave.QueryDefinition.Terms))
		} else {
			Trace.Printf("Not saving query terms. Total terms: %d\n", len(config.QueryDefinition.Terms))
			configToSave = config.Copy()
			if args.Present() {
				if len(config.QueryDefinition.Terms) > 1 {
					config.QueryDefinition.Terms = append(config.QueryDefinition.Terms, "AND")
					config.QueryDefinition.Terms = append(config.QueryDefinition.Terms, args...)
				} else {
					config.QueryDefinition.Terms = []string{args.First()}
					config.QueryDefinition.Terms = append(config.QueryDefinition.Terms, args.Tail()...)
				}
			}
		}

		tail := NewTail(config)

		//If we don't exit here we can save the defaults
		configToSave.SaveDefault()

		tail.Start(!config.IsListOnly(), config.InitialEntries)
	}

	app.Run(os.Args)
}

// Must is a helper function to avoid boilerplate error handling for regex matches
// this way they may be used in single value context
func Must(result bool, err error) bool {
	if err != nil {
		Error.Panic(err)
	}
	return result
}

// Read password from the console
func readPasswd() string {
	bytePassword, err := terminal.ReadPassword(0)
	if err != nil {
		Error.Fatalln("Failed to read password.")
	}
	fmt.Println()
	return string(bytePassword)
}

// EvaluateExpression Expression evaluation function. It uses map as a model and evaluates expression given as
// the parameter using dot syntax:
// "foo" evaluates to model[foo]
// "foo.bar" evaluates to model[foo][bar]
// If a key given in the expression does not exist in the model, function will return empty string and
// an error.
func EvaluateExpression(model interface{}, fieldExpression string) (string, error) {
	if fieldExpression == "" {
		return fmt.Sprintf("%v", model), nil
	}
	var nextModel interface{}
	nextExpression := ""
	modelMap, ok := model.(map[string]interface{})
	if ok {
		value := modelMap[fieldExpression]
		if value != nil {
			nextModel = value
		} else {
			parts := strings.SplitN(fieldExpression, ".", 2)
			expression := parts[0]
			if len(parts) > 1 {
				nextExpression = parts[1]
			}
			value = modelMap[expression]
			if value != nil {
				nextModel = value
			} else {
				return "", fmt.Errorf("Failed to evaluate expression %s on given model %+v (model map does not contain that key?).", fieldExpression, modelMap)
			}
		}
	} else {
		return "", fmt.Errorf("Model on which %s is to be evaluated is not a map.", fieldExpression)
	}
	return EvaluateExpression(nextModel, nextExpression)
}

type KibanaDecorator struct {
	r             http.RoundTripper
	kibanaVersion string
	extraHeaders  map[string]string
	configuration *configuration.Configuration
	cookie        AuthToken
}

func (mrt KibanaDecorator) RoundTrip(r *http.Request) (*http.Response, error) {
	mrt.cookie = LoadToken(mrt.configuration)
	if strings.Contains(r.URL.Path, "_msearch") {
		r.URL.Path = "/elasticsearch/_msearch"
		r.Method = "POST"

		if mrt.kibanaVersion != "" {
			r.Header.Add("kbn-version", mrt.kibanaVersion)
		}

		if mrt.cookie.token != "" {
			r.AddCookie(&http.Cookie{
				//HttpOnly: true,
				Name:  "sid-auth",
				Value: mrt.cookie.token,
			})
		}

		for k, v := range mrt.extraHeaders {
			r.Header.Add(k, v)
		}

		q := r.URL.Query()
		//q.Add("rest_total_hits_as_int", "true")
		//q.Add("ignore_throttled", "true")
		r.URL.RawQuery = q.Encode()
	}
	response, e := mrt.r.RoundTrip(r)

	if e == nil && response.StatusCode == 302 && response.Header.Get("location") == "/login" {
		e = mrt.cookie.Authenticate()
		Error.Fatalln("Failed to authenticate. Please run again. If problem still occurs you have authenticate by passing valid credentials with -u flag")
	}

	return response, e
}

type AuthToken struct {
	config *configuration.Configuration
	token  string
}

func userHomeDir() string {
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home
	}
	return os.Getenv("HOME")
}

var confDir = ".elktail"
var defaultConfFile = "default.json"

func LoadToken(config *configuration.Configuration) AuthToken {
	confDirPath := userHomeDir() + string(os.PathSeparator) + confDir
	confFile := confDirPath + string(os.PathSeparator) + "auth.cookie"
	tokenBytes, err := ioutil.ReadFile(confFile)

	if err != nil {
		token := AuthToken{config: config}
		err = token.Authenticate()
		return token
	}

	return AuthToken{
		config: config,
		token:  string(tokenBytes),
	}
}

func (ths *AuthToken) Authenticate() error {
	request, e := http.NewRequest("POST", ths.config.SearchTarget.Url+"/login", strings.NewReader(url.Values{
		"username": []string{ths.config.User},
		"password": []string{ths.config.Password},
	}.Encode()))

	if e == nil {
		request.Header.Add("kbn-version", "6.2.4")
		request.Header.Add("User-Agent", "elktail")
		request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}}

	response, e := client.Do(request)
	//response, e := http.PostForm(ths.config.SearchTarget.Url+"/login", url.Values{
	//	"username": []string{ths.config.User},
	//	"password": []string{ths.config.Password},
	//})
	if e != nil {
		return e
	}

	for _, v := range response.Cookies() {
		if v.Name == "sid-auth" {
			ths.token = v.Value
		}
	}

	if ths.token == "" {
		return fmt.Errorf("bad credentials")
	}

	confDirPath := userHomeDir() + string(os.PathSeparator) + confDir
	tokenFile := confDirPath + string(os.PathSeparator) + "auth.cookie"
	return ioutil.WriteFile(tokenFile, []byte(ths.token), 0700)
}

func ResolveKibanaVersion(url string, extraHeaders map[string]string) (string, error) {
	return "6.2.4", nil //TODO implement dynamic version resolving based on status endpoint
	//req, _ := http.NewRequest("GET", url + "/api/status", nil)
	//
	//for k, v := range extraHeaders {
	//	req.Header.Set(k, v)
	//}
	//
	//resp, err := http.DefaultClient.Do(req)
	//if err != nil {
	//	return "", err
	//}
	//
	//statusResponse := new(StatusResponse)
	//err = json.NewDecoder(resp.Body).Decode(&statusResponse)
	//
	//if err != nil {
	//	return "", err
	//}
	//
	//return statusResponse.version.number, nil
}

type StatusResponse struct {
	version VersionResponse
}

type VersionResponse struct {
	number string
}

func ExtractHeader(s string) []string {
	split := strings.Split(s, ":")
	for k, v := range split {
		split[k] = strings.Trim(v, " ")
	}
	return split
}
