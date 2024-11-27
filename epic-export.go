package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	gochoice "github.com/TwiN/go-choice"
	"github.com/gogf/gf/text/gstr"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	retries   = 3
	numTokens = 5
	logChSize = 15
	pageSize  = 40
	chunkSize = 1024
	wait      = time.Millisecond * 300

	epicHost = "https://store.epicgames.com"
	epicPrfx = "/en-US/p/"
	outFmt   = `<div><a href="%s%s">%s</a><br/><img src="%s"</img></div>
`
	noLinkFmt = `<div><span>%s</span><br/><img src="%s"</img></div>
`
	skipItem = "Skip item"
	noLink   = "No link"
	typeLink = "Type link"
	schByImg = "Search by logo"
	resByImg = "BY LOGO SEARCH"
)

var (
	reRepl  = regexp.MustCompile(`\W+`)
	reLens  = regexp.MustCompile(`"Show less","See more","Show less Similar images","See more Similar images".*?,"(https?://[^"]+)".*?\[\[.*?,"(https?://[^"]+)".*?\[\[.*?,"(https?://[^"]+)"`)
	seps    = []byte(":- ")
	termMtx sync.Mutex
	writer  *bufio.Writer
	logger  = make(chan string, logChSize)
	client  = &http.Client{}
	retryB  = []byte("<title>Just a moment...</title>")
	notFB   = []byte("/en-US/not-found")
)

var pool sync.Pool = sync.Pool{
	New: func() any {
		return &bytes.Buffer{}
	},
}

func getBuf() *bytes.Buffer {
	return pool.Get().(*bytes.Buffer)
}

type appData struct {
	Data data `json:"data"`
}

type data struct {
	Applications []*game `json:"applications"`
}

type game struct {
	Name string `json:"applicationName"`
	Logo string `json:"logo"`
	work *work

	// isFuzzy is true for the second phase is a fuzzy matching and user-picking,
	// in case of no exact match.
	isFuzzy bool
	// schdByImg means if search by logo was already run for this game.
	schdByImg bool
}

func main() {
	input := flag.String("i", "", "input JSON: exported games file path")
	output := flag.String("o", "", "output HTML: result file path")
	flag.Parse()
	mustString(*input, "exported games file path")
	mustString(*output, "result file path")

	fi, err := os.Open(*input)
	must(err, "open games file")
	defer fi.Close()

	fo, err := os.Create(*output)
	must(err, "create result file")
	defer fo.Close()
	writer = bufio.NewWriter(fo)
	writer.WriteString(`<!DOCTYPE html><html lang="en"><head><style>
body{display:flex;flex-wrap:wrap;background:moccasin}div{margin:5px;padding:5px;border:blue 1px solid;text-align:center}
img{width:300px;padding-top:5px}</style><meta charset="utf-8"><title>My Games</title></head><body>
`)
	defer func() {
		writer.WriteString(`</body></html>`)
		writer.Flush()
	}()

	var ad appData
	must(json.NewDecoder(fi).Decode(&ad), "decode games file")
	games := ad.Data.Applications

	var wg sync.WaitGroup
	tokens := make(chan *work, numTokens)
	for range numTokens {
		var work work
		work.items = make([]workItem, 0, pageSize)
		work.display = make([]string, 0, pageSize+2) // skip texts
		tokens <- &work
	}

	go func() {
		defer wg.Done()
		for l := range logger {
			termMtx.Lock()
			log.Println(l)
			termMtx.Unlock()
		}
	}()

	for gi, g := range games {
		games[gi].Name = strings.TrimSpace(g.Name)
		wg.Add(1)
		go func() {
			work := <-tokens
			defer func() {
				tokens <- work
				wg.Done()
			}()

			link, err := gameByName(g.Name)
			if err == nil {
				writer.WriteString(fmt.Sprintf(outFmt, "", link, g.Name, g.Logo))
				return
			}
			logger <- err.Error()

			g.work = work
			time.Sleep(wait)
			if err = g.search(); err == nil {
				return
			}
			logger <- err.Error()
			g.isFuzzy = true
			if err = g.search(); err != nil {
				logger <- err.Error()
			}
		}()
		time.Sleep(wait)
	}
	wg.Wait()
	wg.Add(1)
	close(logger)
	wg.Wait()
	log.Println("done")
}

type workItem struct {
	name string
	link string
	rank int
}

// work contains logic for handling game search. It also works as a token for running only some
// concurrent queries so that epicgames website doesn't block querying.
// It implements sort.Interface to be able to sort search results by rank.
type work struct {
	items   []workItem
	display []string
}

func (w *work) Len() int {
	return len(w.items)
}

func (w *work) Less(i, j int) bool {
	return w.items[i].rank < w.items[j].rank
}

func (w *work) Swap(i, j int) {
	w.items[i], w.items[j] = w.items[j], w.items[i]
	w.display[i], w.display[j] = w.display[j], w.display[i]
}

// search processes the whole search for a given "app" game.
func (g *game) search() error {
	name := g.Name
	var err error
	if g.isFuzzy {
		name, err = strUntil(g.Name)
		if err != nil {
			logger <- err.Error()
			name = g.Name
		}
	}
	work := g.work
	work.items = work.items[:0]
	work.display = work.display[:0]
	escName := url.QueryEscape(name)
	link := fmt.Sprintf("%s/en-US/browse?q=%s&sortBy=relevancy&sortDir=DESC&count=%d",
		epicHost, escName, pageSize)

	buf, err := epicGet(link)
	if err != nil {
		return fmt.Errorf("failed to search %s: %w", link, err)
	}

	doc, err := goquery.NewDocumentFromReader(buf)
	if err != nil {
		return fmt.Errorf("search document failed for url %s: %w", link, err)
	}

	lis := doc.Find("section > section > ul")
	if lis == nil || len(lis.Nodes) == 0 {
		return g.choice(fmt.Errorf("no ul element found %s: %v", link, lis))
	}
	doc = goquery.NewDocumentFromNode(lis.Nodes[0])
	if lis = doc.Find("li"); lis == nil || len(lis.Nodes) == 0 {
		return g.choice(fmt.Errorf("no li elements found %s: %v", link, lis))
	}

	for i, li := range lis.Nodes {
		var wi workItem
		li, err = nthChildren(li, nthChild{atom.Div, 1}, nthChild{atom.Div, 1}, nthChild{atom.A, 1})
		if err != nil {
			return g.choice(fmt.Errorf("nthChildren failure %d: %w", i, err))
		}
		for _, at := range li.Attr {
			switch at.Key {
			case "aria-label":
				parts := strings.Split(at.Val, ", ")
				if len(parts) == 3 {
					wi.name = parts[1]
				} else {
					wi.name = parts[2]
				}
			case "href":
				wi.link = at.Val
			}
		}
		if len(wi.name) == 0 {
			return g.choice(fmt.Errorf("aria-label not found in attr %#v", li.Attr))
		}
		if len(wi.link) == 0 {
			return g.choice(fmt.Errorf("href not found in attr %#v", li.Attr))
		}
		if !g.isFuzzy && wi.name == name {
			writer.WriteString(fmt.Sprintf(outFmt, epicHost, wi.link, g.Name, g.Logo))
			return nil
		}
		// substrings come first
		if !subAny(wi.name, name) {
			// then we rank the list by Levenshtein distance
			wi.rank = gstr.Levenshtein(wi.name, name, 1, 1, 1)
		}
		work.items = append(work.items, wi)
		work.display = append(work.display, fmt.Sprintf("%s; %s%s", wi.name, epicHost, wi.link))
		// game name doesn't match, check next one
	}
	sort.Sort(work)
	// no exact match, pick
	return g.choice(nil)
}

// choice handles previous error and initiates choosing from the search result list.
func (g *game) choice(err error) error {
	if err != nil {
		if !g.isFuzzy {
			return err
		}
		logger <- err.Error()
	}
	work := g.work
	if len(work.display) == 0 {
		if err = g.searchByImg(); err != nil {
			logger <- err.Error()
		}
	}
	return g.pick()
}

// pick asks the user to choose from the given search result games that matches the "app".
func (g *game) pick() error {
	work := g.work
	if !g.schdByImg {
		work.display = append(work.display, schByImg)
	}
	work.display = append(work.display, noLink, typeLink, skipItem)

	termMtx.Lock()
	choice, index, err := gochoice.Pick(fmt.Sprintf("pick one for %s", g.Name), work.display)
	termMtx.Unlock()
	if err != nil {
		return fmt.Errorf("you didn't select anything: %w", err)
	}
	switch choice {
	case skipItem:
		return nil
	case noLink:
		writer.WriteString(fmt.Sprintf(noLinkFmt, g.Name, g.Logo))
		return nil
	case typeLink:
		var link string
		termMtx.Lock()
		fmt.Printf("type a link for %s:\n", g.Name)
		fmt.Scanln(&link)
		termMtx.Unlock()
		link = strings.TrimSpace(link)
		if len(link) > 0 {
			writer.WriteString(fmt.Sprintf(outFmt, "", link, g.Name, g.Logo))
			return nil
		}
		return fmt.Errorf("you didn't type anything for %s, skipping", g.Name)
	case schByImg:
		if err = g.searchByImg(); err != nil {
			logger <- err.Error()
		}
		work.display = work.display[:len(work.items)]
		return g.pick()
	}
	workItem := work.items[index]
	if len(workItem.name) > 0 {
		writer.WriteString(fmt.Sprintf(outFmt, epicHost, workItem.link, g.Name, g.Logo))
	} else {
		writer.WriteString(fmt.Sprintf(outFmt, "", workItem.link, g.Name, g.Logo))
	}
	return nil
}

// gameByName checks if the "app name" matches the epicgames url.
func gameByName(name string) (string, error) {
	linkName := strings.ToLower(name)
	linkName = reRepl.ReplaceAllString(linkName, "-")
	link := fmt.Sprintf("%s%s%s", epicHost, epicPrfx, linkName)

	buf, err := epicGet(link)
	if err != nil {
		return "", fmt.Errorf("failed to get request with naaive link by %s: %w", linkName, err)
	}
	defer pool.Put(buf)
	if bytes.Contains(buf.Bytes(), notFB) {
		return "", fmt.Errorf("naaive link doesn't work for %s", name)
	}

	return link, nil
}

// epicGet is a hack for HTTP GET from epicgames.com executing command line curl, because go's
// HTTP response status is always 403 Forbidden even with the headers copied from the browser.
// It does a retry on failure with exponential backoff.
func epicGet(link string) (stdout *bytes.Buffer, err error) {
	delay := wait
	for i := 0; i < retries; i++ {
		c := exec.Command("curl", link, "-H",
			"accept: text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
			"-H", "accept-language: en-CA,en;q=0.9",
			"-H", "cache-control: no-cache",
			"-H", "dnt: 1",
			"-H", "pragma: no-cache",
			"-H", "priority: u=0, i",
			"-H", `sec-ch-ua: "Not;A=Brand";v="24", "Chromium";v="128"`,
			"-H", "sec-ch-ua-mobile: ?0",
			"-H", `sec-ch-ua-platform: "Linux"`,
			"-H", "sec-fetch-dest: document",
			"-H", "sec-fetch-mode: navigate",
			"-H", "sec-fetch-site: none",
			"-H", "sec-fetch-user: ?1",
			"-H", "upgrade-insecure-requests: 1",
			"-H", "user-agent: Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
		stdout = getBuf()
		c.Stdout = stdout
		if err = c.Run(); err != nil {
			pool.Put(stdout)
			return nil, err
		}
		b := stdout.Bytes()
		if bytes.Contains(b, notFB) || !bytes.Contains(b, retryB) {
			return stdout, nil
		}
		time.Sleep(delay)
		delay *= 2
		pool.Put(stdout)
	}
	if err = ioutil.WriteFile(fmt.Sprintf("/tmp/epic%s.html", strings.ReplaceAll(link, "/", "-")), stdout.Bytes(), 0777); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("too many retries for %s", link)
}

// httpGet does an HTTP GET request to the given url, and returns the body io.Reader on success.
func httpGet(link string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to http.Do NewRequest %s: %w", link, err)
	}
	req.Header.Set("accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("accept-language", "en-CA,en;q=0.9")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("dnt", "1")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("priority", "u=0, i")
	req.Header.Set("sec-ch-ua", `"Not;A=Brand";v="24", "Chromium";v="128"`)
	req.Header.Set("sec-ch-ua-arch", `"x86"`)
	req.Header.Set("sec-ch-ua-bitness", `"64"`)
	req.Header.Set("sec-ch-ua-form-factors", `"Desktop"`)
	req.Header.Set("sec-ch-ua-full-version", `"128.0.6613.119"`)
	req.Header.Set("sec-ch-ua-full-version-list", `"Not;A=Brand";v="24.0.0.0", "Chromium";v="128.0.6613.119"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-model", `""`)
	req.Header.Set("sec-ch-ua-platform", `"Linux"`)
	req.Header.Set("sec-ch-ua-platform-version", `"6.12.0"`)
	req.Header.Set("sec-ch-ua-wow64", "?0")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-site", "none")
	req.Header.Set("sec-fetch-user", "?1")
	req.Header.Set("upgrade-insecure-requests", "1")
	req.Header.Set("user-agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to http.Do GET %s: %w", link, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wrong status for getting %s: %s", link, resp.Status)
	}
	return resp.Body, nil
}

// searchByImg searches by game logo and fills in display list on success.
func (g *game) searchByImg() error {
	g.schdByImg = true
	body, err := httpGet(fmt.Sprintf("https://lens.google.com/uploadbyurl?url=%s&hl=en-CA", g.Logo))
	if err != nil {
		return err
	}
	defer body.Close()
	b := getBuf()
	defer pool.Put(b)
	b.Grow(chunkSize << 1)
	done, err := g.readLens(body, b, 0)
	bs := b.Bytes()
	if done || err != nil {
		return err
	}
	for {
		cut := b.Cap() - chunkSize
		copy(bs, bs[cut:])
		if done, err = g.readLens(body, b, chunkSize); done || err != nil {
			return err
		}
	}
}

// readLens reads next chunk of HTTP response of Google Images search for matches. Returns true for leaving.
// When found up to 3 distinct results, fills in work's display list.
func (g *game) readLens(body io.Reader, b *bytes.Buffer, readIdx int) (bool, error) {
	b.Truncate(readIdx)
	_, err := b.ReadFrom(body)
	if err != nil {
		return true, fmt.Errorf("failed to read google lens result for %s", g.Name)
	}
	res := reLens.FindSubmatch(b.Bytes())
	if len(res) < 2 {
		return false, nil
	}
	work := g.work
	m := map[string]struct{}{} // keep track of duplicated links
	for _, resi := range res[1:] {
		link := string(resi)
		if _, ok := m[link]; ok {
			continue
		}
		m[link] = struct{}{}
		workItem := workItem{name: "", link: link}
		work.items = append(work.items, workItem)
		name := fmt.Sprintf("%s; %s", resByImg, link)
		if len(work.display) > len(work.items) {
			// replace "search by image"
			work.display[len(work.items)-1] = name
		} else {
			work.display = append(work.display, name)
		}
	}
	return true, nil
}

type nthChild struct {
	tag   atom.Atom
	index int
}

// nthChildren loops on the given HTML tag-index pairs, going down the tree for the specified child.
func nthChildren(n *html.Node, tags ...nthChild) (*html.Node, error) {
	for i, t := range tags {
		if n = n.FirstChild; n == nil {
			return nil, fmt.Errorf("no first child before %dth child %v", i, t.tag)
		}
		for j := range t.index - 1 {
			if n = n.NextSibling; n == nil {
				return nil, fmt.Errorf("no %dth child before %dth child %v", j, i, t.tag)
			}
		}
		if n.DataAtom != t.tag {
			return nil, fmt.Errorf("expected tag %s != %s for nth child before %dth child %v", t.tag, n.DataAtom, i, t.tag)
		}
	}
	return n, nil
}

// subAny returns true if any of the strings is the substring of the other.
func subAny(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	return strings.Contains(a, b)
}

// strUntil cuts game end after the first :, - or ' ' character.
func strUntil(name string) (string, error) {
	for _, s := range seps {
		if idx := strings.IndexByte(name, s); idx > -1 {
			return strings.TrimSpace(name[:idx]), nil
		}
	}
	return "", fmt.Errorf("%s: no separator found in %s", name, seps)
}

// mustString is used for exiting on missing required input arguments.
func mustString(in, descr string) {
	if len(in) == 0 {
		fmt.Printf("%s must be set\n", descr)
		flag.Usage()
		os.Exit(1)
	}
}

func must(err error, descr string) {
	if err != nil {
		panic(fmt.Errorf("%s: %w", descr, err))
	}
}
