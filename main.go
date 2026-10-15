package main

import (
	"cmp"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	_ "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type skip []*regexp.Regexp

func (s *skip) String() string {
	return fmt.Sprintf("%q", (*[]*regexp.Regexp)(s))
}

func (s *skip) Set(v string) error {
	reg, err := regexp.Compile(v)
	if err != nil {
		return err
	}
	*s = append(*s, reg)
	return nil
}

func (s skip) match(path string) bool {
	for i := range s {
		if s[i].MatchString(path) {
			return true
		}
	}
	return false
}

var skipFlag skip

var dir = flag.String("dir", "", "the `directory` dol will use to put files")
var site = flag.String("site", "", "the `url` you will read")

func init() {
	flag.Var(&skipFlag, "skip",
		"a `regular expression` for pages that must be skipped. Can be used multiple times "+
			"for using various regular expressions. Only the path of the url is used when matching.")
}

var baseURL *url.URL

func main() {
	flag.Parse()

	if *dir == "" || *site == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	var err error
	baseURL, err = url.Parse(*site)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(2)
	}

	_, err = os.Stat(filepath.Join(*dir, "visited.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.MkdirAll(*dir, os.ModeDir); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(4)
		}
		if err = createFiles(); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(5)
		}
		serve()
		return
	}
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(6)
	}

	serve()
}

func createFiles() error {
	var order []*url.URL

	u, err := url.Parse(baseURL.Scheme + "://" + baseURL.Host + "/")
	if err != nil {
		return err
	}

	err = walk(u,
		func(u *url.URL, missing int) {
			order = append(order, u)
			fmt.Printf("%-60s %d\n", u.Path, missing)
		},
	)
	if err != nil {
		return err
	}

	if err := createOrderFile(order); err != nil {
		return err
	}
	return createVisitedFile()
}

func createOrderFile(order []*url.URL) error {
	f, err := os.Create(filepath.Join(*dir, "order.txt"))
	if err != nil {
		return err
	}

	for i, u := range order {
		if i > 0 {
			if _, err := f.WriteString("\n"); err != nil {
				f.Close()
				return err
			}
		}
		if _, err := f.WriteString(u.Path); err != nil {
			f.Close()
			return err
		}
	}

	return f.Close()
}

func createVisitedFile() error {
	f, err := os.Create(filepath.Join(*dir, "visited.txt"))
	if err != nil {
		return err
	}
	return f.Close()
}

func walk(u *url.URL, orderFn func(u *url.URL, missing int)) (err error) {
	stack, visitedPaths, hasPreviousState, err := loadWalkState(orderFn)
	if err != nil {
		return err
	}

	walkStateFile, err := os.OpenFile(filepath.Join(*dir, "walk.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	defer func() {
		closeErr := walkStateFile.Close()
		err = cmp.Or(err, closeErr)
	}()

	getLinks := func(u *url.URL) ([]*url.URL, error) {
		resp, err := http.Get(u.Scheme + "://" + u.Host + u.Path)
		if err != nil {
			return nil, err
		}

		n, err := html.Parse(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		return links(u, n), nil
	}

	canVisit := func(u *url.URL) (bool, error) {
		if slices.Contains(visitedPaths, u.Path) || skipFlag.match(u.Path) {
			return false, nil
		}
		visitedPaths = append(visitedPaths, u.Path)
		if !strings.HasSuffix(u.Path, "/") {
			visitedPaths = append(visitedPaths, u.Path+"/")
		}

		resp, err := http.Head(u.Scheme + "://" + u.Host + u.Path)
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html"), nil
	}

	push := func(links ...*url.URL) error {
		for _, u := range slices.Backward(links) {
			stack = append(stack, u)
			if _, err = walkStateFile.WriteString("A " + u.EscapedPath() + "\n"); err != nil {
				return err
			}
		}
		if err = walkStateFile.Sync(); err != nil {
			return err
		}
		return nil
	}

	pop := func() (*url.URL, error) {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, err = walkStateFile.WriteString("R\n"); err != nil {
			return nil, err
		}
		return u, nil
	}

	if !hasPreviousState {
		if err = push(u); err != nil {
			return err
		}
	}

	for {
		if len(stack) == 0 {
			break
		}

		u, err := pop()
		if err != nil {
			return err
		}

		if ok, err := canVisit(u); err != nil {
			return err
		} else if !ok {
			continue
		}

		orderFn(u, len(stack))
		if _, err = walkStateFile.WriteString("O\n"); err != nil {
			return err
		}

		links, err := getLinks(u)
		if err != nil {
			return err
		}

		if err = push(links...); err != nil {
			return err
		}
	}

	return nil
}

func links(u *url.URL, n *html.Node) (ls []*url.URL) {
	for n := range n.Descendants() {
		if n.Type != html.ElementNode || n.DataAtom != atom.A {
			continue
		}
		for _, a := range n.Attr {
			if a.Key != "href" {
				continue
			}
			link, err := u.Parse(a.Val)
			if err != nil {
				break
			}
			if link.Host != u.Host {
				break
			}
			if slices.ContainsFunc(ls, func(e *url.URL) bool { return e.Path == link.Path }) {
				break
			}
			ls = append(ls, link)
			break
		}
	}

	return ls
}

func loadWalkState(orderFn func(u *url.URL, missing int)) (stack []*url.URL, visitedPaths []string, has bool, err error) {
	f, err := os.Open(filepath.Join(*dir, "walk.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return
	}
	defer f.Close()

	var b strings.Builder
	if _, err = io.Copy(&b, f); err != nil {
		return
	}

	var lastRemoved *url.URL
	var missing int

	for line := range strings.Lines(b.String()) {
		line = strings.TrimSpace(line)
		r, _ := utf8.DecodeRuneInString(line)
		switch r {
		case 'A':
			line = strings.TrimPrefix(line, "A ")
			u, err := url.Parse(baseURL.Scheme + "://" + baseURL.Host + line)
			if err != nil {
				return nil, nil, false, err
			}
			stack = append(stack, u)
		case 'R':
			lastRemoved = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			missing = len(stack)
			visitedPaths = append(visitedPaths, lastRemoved.Path)
		case 'O':
			orderFn(lastRemoved, missing)
		}
	}

	return stack, visitedPaths, true, nil
}

//go:embed front/*
var frontFS embed.FS

func serve() {
	http.HandleFunc("GET /api/next", next)
	http.HandleFunc("GET /api/current", current)
	front, err := fs.Sub(frontFS, "front")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("GET /", http.FileServerFS(front))

	fmt.Println("listening on port 3445")
	log.Fatal(http.ListenAndServe("localhost:3445", nil))
}

func next(rw http.ResponseWriter, req *http.Request) {
	current := req.URL.Query().Get("current")
	if current == "" {
		http.Error(rw, "current is required", http.StatusBadRequest)
		return
	}

	if !canCallNext() {
		nextURL := current
		hdr := rw.Header()
		hdr.Add("Content-Type", "text/plain")
		hdr.Add("Content-Length", strconv.Itoa(len(nextURL)))
		if _, err := rw.Write([]byte(nextURL)); err != nil {
			log.Println(err)
		}
		return
	}

	currentURL, err := url.Parse(current)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	if currentURL.Host != baseURL.Host {
		http.Error(rw, "invalid current: "+current, http.StatusBadRequest)
		return
	}

	currentPath := currentURL.Path

	order, err := readOrderFile()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	visited, err := readVisitedFile()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	if !slices.Contains(visited, currentPath) {
		isFirst := len(visited) == 0
		visited = append(visited, currentPath)
		if err = appendToVisitedFile(currentPath, isFirst); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	ind := slices.Index(order, currentPath)
	if ind < 0 {
		http.Error(rw, "invalid current: "+current, http.StatusBadRequest)
		return
	}

	var next string
	for i := ind + 1; i < len(order); i++ {
		if !slices.Contains(visited, order[i]) {
			next = order[i]
			break
		}
	}

	if next == "" {
		http.Error(rw, "All done! Congratulations!", http.StatusBadRequest)
		return
	}

	nextRawURL := baseURL.Scheme + "://" + baseURL.Host + next

	hdr := rw.Header()
	hdr.Add("Content-Type", "text/plain")
	hdr.Add("Content-Length", strconv.Itoa(len(nextRawURL)))
	if _, err = rw.Write([]byte(nextRawURL)); err != nil {
		log.Println(err)
	}
}

var lastNextTimeMutex sync.Mutex
var lastNextTime time.Time = time.Now().Add(-5 * time.Minute)

func canCallNext() bool {
	lastNextTimeMutex.Lock()
	if time.Since(lastNextTime) < 20*time.Second {
		lastNextTimeMutex.Unlock()
		return false
	}
	lastNextTime = time.Now()
	lastNextTimeMutex.Unlock()
	return true
}

func readOrderFile() ([]string, error) {
	f, err := os.Open(filepath.Join(*dir, "order.txt"))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if _, err = io.Copy(&b, f); err != nil {
		return nil, err
	}

	return strings.Split(b.String(), "\n"), nil
}

func readVisitedFile() ([]string, error) {
	f, err := os.Open(filepath.Join(*dir, "visited.txt"))
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if _, err = io.Copy(&b, f); err != nil {
		return nil, err
	}

	return strings.Split(b.String(), "\n"), nil
}

func appendToVisitedFile(path string, isFirst bool) error {
	f, err := os.OpenFile(filepath.Join(*dir, "visited.txt"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if !isFirst {
		if _, err := f.WriteString("\n"); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.WriteString(path); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func current(rw http.ResponseWriter, req *http.Request) {
	order, err := readOrderFile()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	visited, err := readVisitedFile()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	var current string
	for i := range order {
		if !slices.Contains(visited, order[i]) {
			current = baseURL.Scheme + "://" + baseURL.Host + order[i]
			break
		}
	}

	if current == "" {
		http.Error(rw, "All done! Congratulations!", http.StatusBadRequest)
		return
	}

	hdr := rw.Header()
	hdr.Add("Content-Type", "text/plain")
	hdr.Add("Content-Length", strconv.Itoa(len(current)))
	if _, err = rw.Write([]byte(current)); err != nil {
		log.Println(err)
	}
}
