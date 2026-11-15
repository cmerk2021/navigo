package cache

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // register decoders
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sync"
)

// CoverFetcher downloads cover art bytes.
type CoverFetcher func(ctx context.Context, id string, size int) ([]byte, error)

// Covers is a disk + in-memory LRU cache of decoded cover images.
type Covers struct {
	dir string
	max int

	mu       sync.Mutex
	fetch    CoverFetcher
	lru      *list.List
	items    map[string]*list.Element
	inflight map[string][]func()
	failed   map[string]bool
	sem      chan struct{}
}

type coverItem struct {
	key string
	img *image.RGBA
}

// NewCovers creates a cover cache in dir holding up to maxItems decoded
// images in memory.
func NewCovers(dir string, maxItems int) *Covers {
	os.MkdirAll(dir, 0o700)
	return &Covers{
		dir:      dir,
		max:      maxItems,
		lru:      list.New(),
		items:    map[string]*list.Element{},
		inflight: map[string][]func(){},
		failed:   map[string]bool{},
		sem:      make(chan struct{}, 4),
	}
}

// SetFetcher sets the network fetcher.
func (c *Covers) SetFetcher(f CoverFetcher) {
	c.mu.Lock()
	c.fetch = f
	c.failed = map[string]bool{}
	c.mu.Unlock()
}

func coverKey(id string, size int) string { return fmt.Sprintf("%s_%d", sanitize(id), size) }

// Path returns the disk path for a cover (it may not exist yet).
func (c *Covers) Path(id string, size int) string {
	return filepath.Join(c.dir, coverKey(id, size)+".img")
}

// Peek returns the image if it is in memory, without loading.
func (c *Covers) Peek(id string, size int) *image.RGBA {
	if id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[coverKey(id, size)]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*coverItem).img
	}
	return nil
}

// Request returns the image if in memory; otherwise it starts loading it in
// the background (disk, then network) and calls done when finished.
func (c *Covers) Request(id string, size int, done func()) *image.RGBA {
	if id == "" {
		return nil
	}
	if img := c.Peek(id, size); img != nil {
		return img
	}
	key := coverKey(id, size)
	c.mu.Lock()
	if c.failed[key] {
		c.mu.Unlock()
		return nil
	}
	if cbs, ok := c.inflight[key]; ok {
		c.inflight[key] = append(cbs, done)
		c.mu.Unlock()
		return nil
	}
	c.inflight[key] = []func(){done}
	fetch := c.fetch
	c.mu.Unlock()
	go func() {
		c.sem <- struct{}{}
		img, err := c.load(id, size, fetch)
		<-c.sem
		c.mu.Lock()
		cbs := c.inflight[key]
		delete(c.inflight, key)
		if err != nil {
			c.failed[key] = true
		} else {
			c.put(key, img)
		}
		c.mu.Unlock()
		for _, cb := range cbs {
			if cb != nil {
				cb()
			}
		}
	}()
	return nil
}

// Load synchronously loads a cover (disk or network) and returns its path.
func (c *Covers) Load(ctx context.Context, id string, size int) (*image.RGBA, string, error) {
	if img := c.Peek(id, size); img != nil {
		return img, c.Path(id, size), nil
	}
	c.mu.Lock()
	fetch := c.fetch
	c.mu.Unlock()
	img, err := c.load(id, size, fetch)
	if err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	c.put(coverKey(id, size), img)
	c.mu.Unlock()
	return img, c.Path(id, size), nil
}

func (c *Covers) put(key string, img *image.RGBA) {
	if el, ok := c.items[key]; ok {
		el.Value.(*coverItem).img = img
		c.lru.MoveToFront(el)
		return
	}
	c.items[key] = c.lru.PushFront(&coverItem{key: key, img: img})
	for c.lru.Len() > c.max {
		el := c.lru.Back()
		c.lru.Remove(el)
		delete(c.items, el.Value.(*coverItem).key)
	}
}

func (c *Covers) load(id string, size int, fetch CoverFetcher) (*image.RGBA, error) {
	path := c.Path(id, size)
	data, err := os.ReadFile(path)
	if err != nil {
		if fetch == nil {
			return nil, fmt.Errorf("no cover source")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30e9)
		defer cancel()
		data, err = fetch(ctx, id, size)
		if err != nil {
			return nil, err
		}
		tmp := path + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			os.Rename(tmp, path)
		}
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return toRGBA(src, size), nil
}

// toRGBA converts to RGBA, downscaling (box filter) if larger than 2*size.
func toRGBA(src image.Image, size int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if size > 0 && (w > size*2 || h > size*2) {
		f := w / size
		if hf := h / size; hf < f {
			f = hf
		}
		if f > 1 {
			return boxDown(src, f)
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func boxDown(src image.Image, f int) *image.RGBA {
	b := src.Bounds()
	full := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(full, full.Bounds(), src, b.Min, draw.Src)
	w, h := b.Dx()/f, b.Dy()/f
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	n := uint32(f * f)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, a uint32
			for yy := 0; yy < f; yy++ {
				o := full.PixOffset(x*f, y*f+yy)
				for xx := 0; xx < f; xx++ {
					r += uint32(full.Pix[o])
					g += uint32(full.Pix[o+1])
					bl += uint32(full.Pix[o+2])
					a += uint32(full.Pix[o+3])
					o += 4
				}
			}
			d := dst.PixOffset(x, y)
			dst.Pix[d], dst.Pix[d+1], dst.Pix[d+2], dst.Pix[d+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return dst
}

// Clear removes all cached cover files and memory.
func (c *Covers) Clear() {
	c.mu.Lock()
	c.lru.Init()
	c.items = map[string]*list.Element{}
	c.failed = map[string]bool{}
	c.mu.Unlock()
	des, _ := os.ReadDir(c.dir)
	for _, de := range des {
		os.Remove(filepath.Join(c.dir, de.Name()))
	}
}
