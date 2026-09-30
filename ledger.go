package main

import (
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 找文件、带缓存读取

const cacheVersion = 1

type cacheEntry struct {
	Size, Mtime int64
	Data        *FileData
}

type cacheFile struct {
	Version int
	Files   map[string]cacheEntry
}

func loadCache(path string) map[string]cacheEntry {
	f, err := os.Open(path)
	if err != nil {
		return map[string]cacheEntry{}
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return map[string]cacheEntry{}
	}
	var c cacheFile
	if gob.NewDecoder(zr).Decode(&c) != nil || c.Version != cacheVersion || c.Files == nil {
		return map[string]cacheEntry{}
	}
	return c.Files
}

// saveCache 先写临时文件再改名；目录写不进去（比如外置盘没挂载）就只提示，不影响出结果。
func saveCache(path string, files map[string]cacheEntry) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "提示：缓存目录不可写，本次不缓存（%v）\n", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-*.tmp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "提示：缓存目录不可写，本次不缓存（%v）\n", err)
		return
	}
	zw := gzip.NewWriter(tmp)
	err = gob.NewEncoder(zw).Encode(cacheFile{Version: cacheVersion, Files: files})
	if err == nil {
		err = zw.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		fmt.Fprintf(os.Stderr, "提示：写缓存失败（%v）\n", err)
	}
}

func findFiles(roots []string) (files, missing []string) {
	for _, r := range roots {
		if st, err := os.Stat(r); err != nil || !st.IsDir() {
			missing = append(missing, r)
			continue
		}
		filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files, missing
}

// Loaded 是全部文件的解析结果，Files 按路径排序。
type Loaded struct {
	Files   []string
	Data    map[string]*FileData
	Missing []string
	Fresh   int
}

func loadAll(roots []string, cachePath string, quiet bool) *Loaded {
	files, missing := findFiles(roots)
	var cache map[string]cacheEntry
	if cachePath != "" {
		cache = loadCache(cachePath)
	} else {
		cache = map[string]cacheEntry{}
	}
	L := &Loaded{Files: files, Data: make(map[string]*FileData, len(files)), Missing: missing}
	type job struct {
		path        string
		size, mtime int64
	}
	var todo []job
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		size, mtime := st.Size(), st.ModTime().Unix()
		if e, ok := cache[f]; ok && e.Size == size && e.Mtime == mtime && e.Data != nil {
			L.Data[f] = e.Data
			continue
		}
		todo = append(todo, job{f, size, mtime})
	}
	if len(todo) > 0 {
		t0 := time.Now()
		var mu sync.Mutex
		var wg sync.WaitGroup
		ch := make(chan job)
		done := 0
		for w := 0; w < runtime.NumCPU(); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					d, err := ParseFile(j.path)
					mu.Lock()
					done++
					if err != nil {
						fmt.Fprintf(os.Stderr, "提示：读不了 %s（%v）\n", j.path, err)
					} else {
						L.Data[j.path] = d
						cache[j.path] = cacheEntry{j.size, j.mtime, d}
					}
					if !quiet && done%100 == 0 {
						fmt.Fprintf(os.Stderr, "  读取 %d/%d 个新文件，%.0fs\n", done, len(todo), time.Since(t0).Seconds())
					}
					mu.Unlock()
				}
			}()
		}
		for _, j := range todo {
			ch <- j
		}
		close(ch)
		wg.Wait()
		L.Fresh = len(todo)
		if cachePath != "" {
			saveCache(cachePath, cache)
		}
	}
	// 读失败的文件不参与后面的统计
	kept := L.Files[:0]
	for _, f := range L.Files {
		if L.Data[f] != nil {
			kept = append(kept, f)
		}
	}
	L.Files = kept
	return L
}

// ---------------------------------------------------------------- 血缘分组与去重

func selfID(f string, d *FileData) string {
	if d.Meta != nil && d.Meta.ID != "" {
		return d.Meta.ID
	}
	return "file:" + f
}

// lineageGroups：并查集把自己的 id、父会话 id、分叉来源 id、文件里出现过的其他会话 id 连成一组。
func lineageGroups(L *Loaded) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, f := range L.Files {
		d := L.Data[f]
		me := selfID(f, d)
		find(me)
		if d.Meta != nil {
			for _, o := range []string{d.Meta.ParentID, d.Meta.ForkedFromID} {
				if o != "" {
					union(me, o)
				}
			}
		}
		for _, o := range d.OtherIDs {
			union(me, o)
		}
	}
	out := make(map[string]string, len(L.Files))
	for _, f := range L.Files {
		out[f] = find(selfID(f, L.Data[f]))
	}
	return out
}

// Uniq 是去重后的一条请求：来自哪个文件、哪条记录。
type Uniq struct {
	File string
	Ev   *Event
}

type DedupStats struct {
	Raw, Dup, Unique, CrossGroup, ReplayOnly int
	ReplayOnlyTokens                         int64
}

type dedupKey struct {
	g           string
	total, last [6]int64
}

type rank struct {
	ts, isFork, metaTS int64
	file               string
}

func (a rank) less(b rank) bool {
	if a.ts != b.ts {
		return a.ts < b.ts
	}
	if a.isFork != b.isFork {
		return a.isFork < b.isFork
	}
	if a.metaTS != b.metaTS {
		return a.metaTS < b.metaTS
	}
	return a.file < b.file
}

// dedupEvents：同一血缘组内按（累计用量 + 本次用量）去重，保留时间最早的原件；
// 时间相同时归给非分叉的会话。输出顺序 = 每个键第一次出现的顺序。
func dedupEvents(L *Loaded, groups map[string]string) ([]Uniq, DedupStats) {
	var st DedupStats
	idx := map[dedupKey]int{}
	var uniq []Uniq
	var ranks []rank
	var keys []dedupKey
	for _, f := range L.Files {
		d := L.Data[f]
		g := groups[f]
		var isFork, metaTS int64
		if d.Meta != nil {
			if d.Meta.ForkedFromID != "" || d.Meta.ParentID != "" {
				isFork = 1
			}
			metaTS = d.Meta.TS
		}
		for i := range d.Events {
			e := &d.Events[i]
			st.Raw++
			k := dedupKey{g, e.Total, e.Last}
			r := rank{e.TS, isFork, metaTS, f}
			if j, ok := idx[k]; ok {
				st.Dup++
				if r.less(ranks[j]) {
					uniq[j], ranks[j] = Uniq{f, e}, r
				}
				continue
			}
			idx[k] = len(uniq)
			uniq = append(uniq, Uniq{f, e})
			ranks = append(ranks, r)
			keys = append(keys, k)
		}
	}
	st.Unique = len(uniq)

	// 质量检查 A：不同血缘组之间出现完全相同的用量 → 可能有没连上的分叉在重复计数
	type usageOnly struct{ total, last [6]int64 }
	seen := map[usageOnly]string{}
	for _, k := range keys {
		if k.total[5] < 1000 {
			continue
		}
		u := usageOnly{k.total, k.last}
		if g, ok := seen[u]; ok && g != k.g {
			st.CrossGroup++
		} else if !ok {
			seen[u] = k.g
		}
	}

	// 质量检查 B：父会话文件已经不在，子会话开头复制来的父历史成了唯一记录
	have := map[string]bool{}
	for _, f := range L.Files {
		if m := L.Data[f].Meta; m != nil {
			have[m.ID] = true
		}
	}
	for _, u := range uniq {
		m := L.Data[u.File].Meta
		if m == nil {
			continue
		}
		p := m.ForkedFromID
		if p == "" {
			p = m.ParentID
		}
		if p != "" && !have[p] && u.Ev.TS-m.TS < 5000 {
			st.ReplayOnly++
			st.ReplayOnlyTokens += u.Ev.Last[0] + u.Ev.Last[3]
		}
	}
	return uniq, st
}
