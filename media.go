package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
)

const (
	maxUploadSize    = 16 << 20
	maxImagePixels   = 50_000_000
	maxProxyImage    = 20 << 20
	maxProxyMedia    = 60 << 20
	proxyCacheMaxAge = 7 * 24 * time.Hour
)

// ---- uploads ----

// saveUpload stores an uploaded image. JPEG/PNG/WebP images are re-encoded,
// which strips EXIF metadata (GPS positions!) after applying the EXIF
// orientation. GIFs are kept as they are (animation).
func (app *App) saveUpload(acc *Account, data []byte, description string) (*Attachment, error) {
	if len(data) > maxUploadSize {
		return nil, errors.New("file is too large (max 16 MB)")
	}
	ct := http.DetectContentType(data)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("unsupported file type (use JPEG, PNG, GIF or WebP)")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxImagePixels || cfg.Width > 16384 || cfg.Height > 16384 {
		return nil, errors.New("image dimensions are too large")
	}
	id := newID()
	var out []byte
	var ext, mediaType string
	w, h := cfg.Width, cfg.Height
	switch format {
	case "gif":
		if _, err := gif.DecodeAll(bytes.NewReader(data)); err != nil {
			return nil, errors.New("broken GIF")
		}
		out, ext, mediaType = data, ".gif", "image/gif"
	case "jpeg", "png", "webp":
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, errors.New("could not decode image")
		}
		if format == "jpeg" {
			img = applyOrientation(img, jpegOrientation(data))
		}
		b := img.Bounds()
		w, h = b.Dx(), b.Dy()
		var buf bytes.Buffer
		opaque := true
		if o, ok := img.(interface{ Opaque() bool }); ok {
			opaque = o.Opaque()
		}
		if format == "png" || !opaque {
			err = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
			ext, mediaType = ".png", "image/png"
		} else {
			err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88})
			ext, mediaType = ".jpg", "image/jpeg"
		}
		if err != nil {
			return nil, err
		}
		out = buf.Bytes()
	default:
		return nil, fmt.Errorf("unsupported image type %s", ct)
	}
	name := id + ext
	if err := os.WriteFile(filepath.Join(app.mediaDir, name), out, 0o640); err != nil {
		return nil, err
	}
	at := &Attachment{
		ID: id, AccountID: acc.ID, URL: app.cfg.BaseURL() + "/media/" + name, MediaType: mediaType,
		Description: truncateRunes(strings.TrimSpace(description), 1500), File: name, Width: w, Height: h,
	}
	_, err = app.db.Exec(`INSERT INTO attachments (id,status_id,account_id,url,media_type,description,file,width,height,created_at) VALUES (?,NULL,?,?,?,?,?,?,?,?)`,
		at.ID, at.AccountID, at.URL, at.MediaType, at.Description, at.File, at.Width, at.Height, time.Now().Unix())
	return at, err
}

func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA {
			break
		}
		size := int(b[i+2])<<8 | int(b[i+3])
		end := min(i+2+size, len(b))
		seg := b[i+4 : end]
		if marker == 0xE1 && len(seg) >= 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 0 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 1
}

// applyOrientation rotates/flips img according to an EXIF orientation value.
func applyOrientation(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	src := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	w, h := b.Dx(), b.Dy()
	var dst *image.NRGBA
	if o >= 5 {
		dst = image.NewNRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewNRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			i := src.PixOffset(x, y)
			j := dst.PixOffset(dx, dy)
			copy(dst.Pix[j:j+4], src.Pix[i:i+4])
		}
	}
	return dst
}

var mediaNameRe = regexp.MustCompile(`^[0-9A-Z]{26}\.(jpg|png|gif)$`)

func (app *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !mediaNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(app.mediaDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	ct := map[string]string{".jpg": "image/jpeg", ".png": "image/png", ".gif": "image/gif"}[filepath.Ext(name)]
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// ---- media proxy ----

// mediaURL maps a (possibly remote) media URL to what the browser should
// load: local files directly, remote ones through our caching proxy so that
// remote servers never see our users' IP addresses.
func (app *App) mediaURL(u string) string {
	if u == "" {
		return ""
	}
	base := app.cfg.BaseURL()
	if strings.HasPrefix(u, base+"/") {
		return strings.TrimPrefix(u, base)
	}
	if !strings.HasPrefix(u, "https://") && !(app.cfg.Insecure && strings.HasPrefix(u, "http://")) {
		return ""
	}
	return "/proxy/" + app.proxySig(u) + "?url=" + url.QueryEscape(u)
}

func (app *App) proxySig(u string) string {
	m := hmac.New(sha256.New, app.proxyKey)
	m.Write([]byte(u))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

var proxyTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
	"video/mp4": true, "video/webm": true, "video/quicktime": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/mp4": true, "audio/wav": true, "audio/x-wav": true,
	"audio/webm": true, "audio/flac": true, "audio/aac": true, "audio/opus": true,
}

var proxyLocks sync.Map

func (app *App) handleProxy(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" || !hmac.Equal([]byte(r.PathValue("sig")), []byte(app.proxySig(u))) {
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	key := sha256Hex(u)
	file := filepath.Join(app.cacheDir, key)
	mu, _ := proxyLocks.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	ct, err := os.ReadFile(file + ".type")
	if err != nil {
		var typ string
		typ, err = app.fetchToCache(r, u, file)
		ct = []byte(typ)
	}
	mu.(*sync.Mutex).Unlock()
	proxyLocks.Delete(key)
	if err != nil {
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.Error(w, "could not fetch media", http.StatusBadGateway)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "cache error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", string(ct))
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (app *App) fetchToCache(r *http.Request, u, file string) (string, error) {
	pu, err := app.checkRemoteURL(u)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, pu.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", app.userAgent())
	resp, err := app.mediaClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	declared := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	limit := int64(maxProxyMedia)
	if strings.HasPrefix(declared, "image/") {
		limit = maxProxyImage
	}
	tmp, err := os.CreateTemp(app.cacheDir, "dl-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	tmp.Close()
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", errors.New("too large")
	}
	head := make([]byte, 512)
	if f, err := os.Open(tmp.Name()); err == nil {
		k, _ := io.ReadFull(f, head)
		head = head[:k]
		f.Close()
	}
	sniffed := strings.SplitN(http.DetectContentType(head), ";", 2)[0]
	typ := ""
	switch {
	case proxyTypes[sniffed]:
		typ = sniffed
	case sniffed == "application/octet-stream" && proxyTypes[declared]:
		typ = declared // e.g. AVIF, which the sniffer doesn't know
	case sniffed == "audio/basic" || strings.HasPrefix(sniffed, "audio/") || strings.HasPrefix(sniffed, "video/"):
		if proxyTypes[declared] {
			typ = declared
		}
	}
	if typ == "" {
		return "", fmt.Errorf("refusing to proxy %s (declared %s)", sniffed, declared)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return "", err
	}
	if err := os.WriteFile(file+".type", []byte(typ), 0o640); err != nil {
		return "", err
	}
	return typ, nil
}

func (app *App) cleanCache(maxAge time.Duration) {
	entries, err := os.ReadDir(app.cacheDir)
	if err != nil {
		return
	}
	cut := time.Now().Add(-maxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.ModTime().Before(cut) {
			os.Remove(filepath.Join(app.cacheDir, e.Name()))
		}
	}
}
