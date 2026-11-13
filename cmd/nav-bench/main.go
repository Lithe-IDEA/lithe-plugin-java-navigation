// Command nav-bench measures a Java language server on a real project: it
// samples identifiers in the project's src/main/java files (not keywords,
// literals, comments, package or import lines), asks the server for the
// definition of each one, then for the references of some, and reports the
// share that resolved per syntactic category, the latency of each request
// and the server's memory. It is how the numbers in the README were taken,
// for this server and for JDT LS on the same samples.
//
//	go run ./cmd/nav-bench -root ~/code/nacos -seed 2 -tag jn -out /tmp/bench -- \
//	    "$(go env GOPATH)/bin/java-navigation" --state-dir /tmp/bench/state
//	go run ./cmd/nav-bench -root ~/code/nacos -seed 2 -jdtls -tag jdtls -out /tmp/bench -- \
//	    jdtls -data /tmp/bench/jdtls-data
//
// It writes <tag>.summary.json, <tag>.results.json (every sample and where it
// went) and <tag>.stderr (the server's log) to -out. The project must be a
// Git working tree. Memory is the resident set of the server and its child
// processes; on macOS the physical footprint, which Activity Monitor shows,
// is reported too. Run it on an idle machine: it measures wall-clock time.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf16"
)

var keywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static strictfp super switch synchronized this throw throws transient try void volatile while true false null var record yield sealed permits non`) {
		keywords[k] = true
	}
}

type sample struct {
	file, uri  string
	line, col  int // col in UTF-16
	name, kind string
	text       string
}

type client struct {
	in      io.WriteCloser
	out     *bufio.Reader
	mu      sync.Mutex
	nextID  int64
	pending sync.Map // id -> chan json.RawMessage
	ready   chan struct{}
	once    sync.Once
	busy    atomic.Int64
	lastMsg atomic.Int64
}

func (c *client) send(v any) {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(b))
	c.in.Write(b)
	c.mu.Unlock()
}

func (c *client) request(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan json.RawMessage, 1)
	c.pending.Store(id, ch)
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		return r, nil
	case <-time.After(timeout):
		c.pending.Delete(id)
		return nil, fmt.Errorf("timeout %s", method)
	}
}

func (c *client) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *client) loop() {
	for {
		n := 0
		for {
			line, err := c.out.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				n, _ = strconv.Atoi(strings.TrimSpace(line[15:]))
			}
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.out, buf); err != nil {
			return
		}
		c.lastMsg.Store(time.Now().UnixNano())
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		json.Unmarshal(buf, &m)
		if m.Method != "" && len(m.ID) > 0 { // server request: answer null
			c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil})
			continue
		}
		if m.Method == "language/status" {
			var p struct{ Type, Message string }
			json.Unmarshal(m.Params, &p)
			if p.Type == "ServiceReady" {
				c.once.Do(func() { close(c.ready) })
			}
			continue
		}
		if m.Method == "$/progress" {
			var p struct {
				Value struct{ Kind string } `json:"value"`
			}
			json.Unmarshal(m.Params, &p)
			switch p.Value.Kind {
			case "begin":
				c.busy.Add(1)
			case "end":
				c.busy.Add(-1)
			}
			continue
		}
		if len(m.ID) > 0 && m.Method == "" {
			id, _ := strconv.ParseInt(string(m.ID), 10, 64)
			if ch, ok := c.pending.LoadAndDelete(id); ok {
				ch.(chan json.RawMessage) <- m.Result
			}
		}
	}
}

// footprintKiB is macOS's physical footprint (what Activity Monitor shows),
// summed over pid and its descendants; RSS keeps pages the process already
// returned with MADV_FREE until the system reclaims them. It is 0 elsewhere.
func footprintKiB(pid int) int {
	if runtime.GOOS != "darwin" {
		return 0
	}
	total := 0
	for _, p := range tree(pid) {
		out, _ := exec.Command("top", "-l", "1", "-stats", "mem", "-pid", strconv.Itoa(p)).Output()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		v := strings.TrimSpace(lines[len(lines)-1])
		mult := 1.0
		switch {
		case strings.HasSuffix(v, "G"):
			mult = 1024 * 1024
		case strings.HasSuffix(v, "M"):
			mult = 1024
		case strings.HasSuffix(v, "K"):
			mult = 1
		case strings.HasSuffix(v, "B"):
			mult = 1.0 / 1024
		}
		f, _ := strconv.ParseFloat(strings.TrimRight(v, "GMKB+-"), 64)
		total += int(f * mult)
	}
	return total
}

func tree(pid int) []int {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	kids := map[int][]int{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		kids[pp] = append(kids[pp], p)
	}
	res := []int{pid}
	for i := 0; i < len(res); i++ {
		res = append(res, kids[res[i]]...)
	}
	return res
}

func rssKiB(pid int) int {
	// Sum the server and its descendants (jdtls is a python wrapper around java).
	out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=").Output()
	kids := map[int][]int{}
	rss := map[int]int{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		r, _ := strconv.Atoi(f[2])
		kids[pp] = append(kids[pp], p)
		rss[p] = r
	}
	total := 0
	var walk func(int)
	walk = func(p int) {
		total += rss[p]
		for _, k := range kids[p] {
			walk(k)
		}
	}
	walk(pid)
	return total
}

var identRe = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)

// mask blanks comments, strings and char literals, keeping offsets.
func mask(src []byte) []byte {
	b := append([]byte(nil), src...)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			for ; i < len(b) && !(b[i] == '*' && i+1 < len(b) && b[i+1] == '/'); i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			if i+1 < len(b) {
				b[i], b[i+1] = ' ', ' '
				i++
			}
		case b[i] == '"' && i+2 < len(b) && b[i+1] == '"' && b[i+2] == '"':
			b[i], b[i+1], b[i+2] = ' ', ' ', ' '
			for i += 3; i+2 < len(b) && !(b[i] == '"' && b[i+1] == '"' && b[i+2] == '"'); i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			if i+2 < len(b) {
				b[i], b[i+1], b[i+2] = ' ', ' ', ' '
				i += 2
			}
		case b[i] == '"' || b[i] == '\'':
			q := b[i]
			b[i] = ' '
			for i++; i < len(b) && b[i] != q && b[i] != '\n'; i++ {
				if b[i] == '\\' {
					b[i] = ' '
					i++
				}
				if i < len(b) {
					b[i] = ' '
				}
			}
			if i < len(b) && b[i] == q {
				b[i] = ' '
			}
		}
	}
	return b
}

func prevNonSpace(s string, i int) (byte, int) {
	for i--; i >= 0; i-- {
		if s[i] != ' ' && s[i] != '\t' {
			return s[i], i
		}
	}
	return 0, -1
}

func nextNonSpace(s string, i int) byte {
	for ; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return s[i]
		}
	}
	return 0
}

func classify(line string, start, end int, name string) string {
	pc, pi := prevNonSpace(line, start)
	nc := nextNonSpace(line, end)
	prevWord := ""
	if pi >= 0 {
		j := pi + 1
		k := j
		for k > 0 && (isIdent(line[k-1])) {
			k--
		}
		prevWord = line[k:j]
	}
	allCaps := strings.ToUpper(name) == name && strings.ContainsAny(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	upper := unicode.IsUpper(rune(name[0]))
	switch {
	case pc == '@':
		return "注解 @X"
	case pc == '.' && nc == '(':
		return "方法调用 a.b()"
	case nc == '(' && prevWord == "new":
		return "构造器 new X()"
	case nc == '(' && (prevWord == "" || prevWord == "return" || pc == '(' || pc == ',' || pc == '=' || pc == '!' || pc == '?' || pc == ':' || pc == '{' || pc == ';' || pc == '+' || pc == '-' || pc == '&' || pc == '|' || prevWord == "this" || prevWord == "super"):
		return "无限定调用 b()"
	case nc == '(':
		return "方法/构造器声明"
	case pc == '.' && allCaps:
		return "常量/枚举 X.Y"
	case pc == '.' && upper:
		return "限定类型 a.B"
	case pc == '.':
		return "字段/成员 a.b"
	case allCaps && len(name) > 1:
		return "常量/枚举 Y"
	case upper:
		return "类型名"
	default:
		return "变量/参数/字段"
	}
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func utf16Col(line string, byteOff int) int {
	return len(utf16.Encode([]rune(line[:byteOff])))
}

func collect(root string) []sample {
	out, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "*.java").Output()
	if err != nil {
		panic(err)
	}
	var all []sample
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.Contains(rel, "src/main/java/") {
			continue
		}
		path := filepath.Join(root, rel)
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		masked := strings.Split(string(mask(src)), "\n")
		orig := strings.Split(string(src), "\n")
		for ln, l := range masked {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "package ") || strings.HasPrefix(t, "import ") {
				continue
			}
			for _, m := range identRe.FindAllStringIndex(l, -1) {
				name := l[m[0]:m[1]]
				if keywords[name] || (m[0] > 0 && l[m[0]-1] >= '0' && l[m[0]-1] <= '9') {
					continue
				}
				all = append(all, sample{file: path, uri: "file://" + path, line: ln, col: utf16Col(orig[ln], m[0]), name: name,
					kind: classify(l, m[0], m[1], name), text: strings.TrimSpace(orig[ln])})
			}
		}
	}
	return all
}

type location struct {
	URI       string `json:"uri"`
	TargetURI string `json:"targetUri"`
	Range     struct {
		Start struct{ Line int } `json:"start"`
	} `json:"range"`
}

func main() {
	root := flag.String("root", "", "project root")
	seed := flag.Int64("seed", 2, "sample seed")
	n := flag.Int("n", 1500, "samples")
	jdtls := flag.Bool("jdtls", false, "wait for JDT LS import")
	tag := flag.String("tag", "run", "output tag")
	outDir := flag.String("out", ".", "output dir")
	refs := flag.Int("refs", 20, "references requests")
	flag.Parse()
	args := flag.Args()
	if *root == "" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: nav-bench -root <project> [-seed n] [-n samples] [-jdtls] [-tag name] [-out dir] -- <server command>")
		os.Exit(2)
	}
	all := collect(*root)
	r := rand.New(rand.NewSource(*seed))
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) > *n {
		all = all[:*n]
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = *root
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	errf, _ := os.Create(filepath.Join(*outDir, *tag+".stderr"))
	cmd.Stderr = errf
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	c := &client{in: in, out: bufio.NewReaderSize(outp, 1<<20), ready: make(chan struct{})}
	go c.loop()
	peak := atomic.Int64{}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				if v := int64(rssKiB(cmd.Process.Pid)); v > peak.Load() {
					peak.Store(v)
				}
			}
		}
	}()
	rootURI := "file://" + *root
	_, err := c.request("initialize", map[string]any{"processId": os.Getpid(), "rootUri": rootURI,
		"workspaceFolders": []any{map[string]any{"uri": rootURI, "name": filepath.Base(*root)}},
		"capabilities": map[string]any{"window": map[string]any{"workDoneProgress": true},
			"textDocument": map[string]any{"definition": map[string]any{"linkSupport": false}}},
		"initializationOptions": map[string]any{"extendedClientCapabilities": map[string]any{"classFileContentsSupport": true, "progressReportProvider": true}}},
		10*time.Minute)
	if err != nil {
		panic(err)
	}
	c.notify("initialized", map[string]any{})
	ready := time.Since(t0)
	if *jdtls {
		select {
		case <-c.ready:
		case <-time.After(20 * time.Minute):
			fmt.Fprintln(os.Stderr, "jdtls never ServiceReady")
		}
		// wait for background builds to settle: no messages for 20s
		for time.Since(time.Unix(0, c.lastMsg.Load())) < 20*time.Second {
			time.Sleep(time.Second)
		}
		ready = time.Since(t0)
	}
	rssReady := rssKiB(cmd.Process.Pid)

	type res struct {
		Kind, Name, Text, File string
		Line, Col              int
		Target                 string
		Ms                     float64
	}
	var results []res
	var first time.Duration
	var rssFirst, fpFirst int
	for i, s := range all {
		st := time.Now()
		raw, err := c.request("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": s.uri},
			"position": map[string]any{"line": s.line, "character": s.col}}, 60*time.Second)
		d := time.Since(st)
		if i == 0 {
			first = d
			rssFirst = rssKiB(cmd.Process.Pid)
			fpFirst = footprintKiB(cmd.Process.Pid)
		}
		target := ""
		if err != nil {
			target = "ERROR " + err.Error()
		} else {
			var locs []location
			if json.Unmarshal(raw, &locs) != nil {
				var one location
				if json.Unmarshal(raw, &one) == nil && (one.URI != "" || one.TargetURI != "") {
					locs = []location{one}
				}
			}
			if len(locs) > 0 {
				u := locs[0].URI
				if u == "" {
					u = locs[0].TargetURI
				}
				target = u + ":" + strconv.Itoa(locs[0].Range.Start.Line+1)
			}
		}
		results = append(results, res{s.kind, s.name, s.text, s.file, s.line + 1, s.col, target, float64(d.Microseconds()) / 1000})
	}
	rssQueries := rssKiB(cmd.Process.Pid)
	// references on hits that land in the project
	var refMs []float64
	for _, x := range results {
		if len(refMs) >= *refs {
			break
		}
		if !strings.HasPrefix(x.Target, rootURI) || x.Kind != "方法调用 a.b()" {
			continue
		}
		st := time.Now()
		c.request("textDocument/references", map[string]any{"textDocument": map[string]any{"uri": "file://" + x.File},
			"position": map[string]any{"line": x.Line - 1, "character": x.Col}, "context": map[string]any{"includeDeclaration": true}}, 120*time.Second)
		refMs = append(refMs, float64(time.Since(st).Microseconds())/1000)
	}
	close(stop)
	rssEnd := rssKiB(cmd.Process.Pid)
	fpEnd := footprintKiB(cmd.Process.Pid)

	type agg struct{ Total, Hit, Project, JDK, Other int }
	byKind := map[string]*agg{}
	var lat []float64
	for _, x := range results {
		a := byKind[x.Kind]
		if a == nil {
			a = &agg{}
			byKind[x.Kind] = a
		}
		a.Total++
		lat = append(lat, x.Ms)
		if x.Target != "" && !strings.HasPrefix(x.Target, "ERROR") {
			a.Hit++
			switch {
			case strings.HasPrefix(x.Target, rootURI):
				a.Project++
			case strings.Contains(x.Target, "jdk") || strings.Contains(x.Target, "java.base") || strings.Contains(x.Target, "/java/"):
				a.JDK++
			default:
				a.Other++
			}
		}
	}
	sort.Float64s(lat)
	sort.Float64s(refMs)
	pct := func(v []float64, p float64) float64 {
		if len(v) == 0 {
			return 0
		}
		return v[int(float64(len(v)-1)*p)]
	}
	summary := map[string]any{"tag": *tag, "samples": len(results), "byKind": byKind,
		"readyMs": ready.Milliseconds(), "firstMs": first.Milliseconds(),
		"p50": pct(lat, .5), "p90": pct(lat, .9), "p99": pct(lat, .99), "max": pct(lat, 1),
		"refP50": pct(refMs, .5), "refMax": pct(refMs, 1), "refN": len(refMs),
		"rssReadyMiB": rssReady / 1024, "rssAfterQueriesMiB": rssQueries / 1024, "rssEndMiB": rssEnd / 1024, "peakMiB": peak.Load() / 1024,
		"rssFirstMiB": rssFirst / 1024, "footprintFirstMiB": fpFirst / 1024, "footprintEndMiB": fpEnd / 1024}
	b, _ := json.MarshalIndent(summary, "", "  ")
	os.WriteFile(filepath.Join(*outDir, *tag+".summary.json"), b, 0o644)
	rb, _ := json.MarshalIndent(results, "", " ")
	os.WriteFile(filepath.Join(*outDir, *tag+".results.json"), rb, 0o644)
	fmt.Println(string(b))
}
