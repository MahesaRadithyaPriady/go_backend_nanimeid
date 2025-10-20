package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	dbpkg "go-nanimeid-video-api/database"

	"github.com/gin-gonic/gin"
)

type Video struct {
	ID        int64     `json:"id"`
	NamaVideo string    `json:"nama_video"`
	Slug      string    `json:"slug"`
	VideoURL  string    `json:"video_url"`
	EncodeHLS bool      `json:"encode_hls"`
	CreatedAt time.Time `json:"created_at"`
}

// --- GPU (NVENC) detection ---
var (
    nvencOnce      sync.Once
    nvencAvailable bool
)

func hasNVENC() bool {
    nvencOnce.Do(func() {
        out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
        if err == nil && strings.Contains(string(out), "h264_nvenc") {
            nvencAvailable = true
        }
    })
    return nvencAvailable
}

// --- upload progress (SSE) ---
var (
    upMu   sync.Mutex
    upSubs = map[string]map[chan string]struct{}{}
    upVals = map[string]int{}
)

func upBroadcast(id string, msg string) {
    upMu.Lock()
    subs := upSubs[id]
    upMu.Unlock()
    for ch := range subs {
        select { case ch <- msg: default: }
    }
}

func upSetPercent(id string, p int) {
    if p < 0 { p = 0 }
    if p > 100 { p = 100 }
    upMu.Lock()
    upVals[id] = p
    upMu.Unlock()
    upBroadcast(id, fmt.Sprintf("data: {\"percent\": %d}\n\n", p))
}

func handleUploadProgressSSE(c *gin.Context) {
    id := c.Param("id")
    if id == "" { c.Status(http.StatusBadRequest); return }
    c.Writer.Header().Set("Content-Type", "text/event-stream")
    c.Writer.Header().Set("Cache-Control", "no-cache")
    c.Writer.Header().Set("Connection", "keep-alive")
    c.Writer.Flush()
    ch := make(chan string, 16)
    upMu.Lock()
    if upSubs[id] == nil { upSubs[id] = map[chan string]struct{}{} }
    upSubs[id][ch] = struct{}{}
    init := upVals[id]
    upMu.Unlock()
    fmt.Fprintf(c.Writer, "data: {\"percent\": %d}\n\n", init)
    c.Writer.Flush()
    notify := c.Request.Context().Done()
    for {
        select {
        case <-notify:
            upMu.Lock(); delete(upSubs[id], ch); upMu.Unlock(); return
        case msg := <-ch:
            io.WriteString(c.Writer, msg)
            c.Writer.Flush()
        }
    }
}

// loadEnvFileLegacy is an earlier variant; kept to avoid redeclaration with the version used below
func loadEnvFileLegacy(path string) {
    b, err := os.ReadFile(path)
    if err != nil { return }
    lines := strings.Split(string(b), "\n")
    for _, ln := range lines {
        s := strings.TrimSpace(ln)
        if s == "" || strings.HasPrefix(s, "#") { continue }
        // split on first '='
        if i := strings.IndexByte(s, '='); i > 0 {
            k := strings.TrimSpace(s[:i])
            v := strings.TrimSpace(s[i+1:])
            // strip optional quotes
            v = strings.Trim(v, "'\"")
            if k != "" { os.Setenv(k, v) }
        }
    }
}

func handleDeleteVideo(c *gin.Context) {
    idStr := c.Param("id")
    id, err := strconv.ParseInt(idStr, 10, 64)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
        return
    }
    var url string
    err = dbpkg.DB.QueryRow("SELECT video_url FROM videos WHERE id=?", id).Scan(&url)
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
        return
    }

    dbpkg.DB.Exec("DELETE FROM videos WHERE id=?", id)

    hlsDir := filepath.Join("./hls", fmt.Sprint(id))
    os.RemoveAll(hlsDir)

    if strings.HasPrefix(url, "/uploads/") {
        os.Remove("." + url)
    } else if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
        if p, err := neturl.Parse(url); err == nil {
            if strings.HasPrefix(p.Path, "/uploads/") {
                os.Remove("." + p.Path)
            }
        }
    } else if strings.Contains(url, "/uploads/") {
        idx := strings.Index(url, "/uploads/")
        if idx >= 0 {
            os.Remove("." + url[idx:])
        }
    }

    c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// --- ffmpeg helpers for progress ---
func runFFmpegWithProgress(args []string, totalDur float64, index int, total int, onProgress func(int)) error {
    cmd := exec.Command("ffmpeg", args...)
    stderr, err := cmd.StderrPipe()
    if err != nil {
        return err
    }
    cmd.Stdout = os.Stdout
    if err := cmd.Start(); err != nil {
        return err
    }
    base := int(float64(index) / float64(total) * 100.0)
    reader := bufio.NewReader(stderr)
    re := regexp.MustCompile(`time=([0-9:.]+)`) // parse time=hh:mm:ss.xx
    // capture last lines of stderr for diagnostics
    const maxLines = 200
    var tail []string
    for {
        line, rerr := reader.ReadString('\n')
        if len(line) > 0 && totalDur > 0 {
            if m := re.FindStringSubmatch(line); len(m) == 2 {
                sec := hmsToSeconds(m[1])
                frac := sec / totalDur
                if frac > 1 {
                    frac = 1
                }
                part := int(frac * (100.0 / float64(total)))
                onProgress(base + part)
            }
        }
        if len(line) > 0 {
            if len(tail) >= maxLines {
                tail = tail[1:]
            }
            tail = append(tail, strings.TrimRight(line, "\n"))
        }
        if rerr != nil {
            break
        }
    }
    if err := cmd.Wait(); err != nil {
        // include args and stderr tail
        return fmt.Errorf("ffmpeg failed: %v\nargs=%v\nstderr tail:\n%s", err, args, strings.Join(tail, "\n"))
    }
    onProgress(int(float64(index+1) / float64(total) * 100.0))
    return nil
}

func hmsToSeconds(s string) float64 {
	// format hh:mm:ss.xx
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	h, _ := strconv.ParseFloat(parts[0], 64)
	m, _ := strconv.ParseFloat(parts[1], 64)
	sec, _ := strconv.ParseFloat(parts[2], 64)
	return h*3600 + m*60 + sec
}

func ffprobeDuration(in string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", in).Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

// --- Progress SSE state ---
var (
	progMu   sync.Mutex
	progSubs = map[int64]map[chan string]struct{}{}
	progVals = map[int64]int{}
)

func progBroadcast(id int64, msg string) {
	progMu.Lock()
	subs := progSubs[id]
	progMu.Unlock()
	for ch := range subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func progSetPercent(id int64, p int) {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	progMu.Lock()
	progVals[id] = p
	progMu.Unlock()
	progBroadcast(id, fmt.Sprintf("data: {\"percent\": %d}\n\n", p))
}

func handleEncodeProgressSSE(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()

	ch := make(chan string, 16)
	progMu.Lock()
	if progSubs[id] == nil {
		progSubs[id] = map[chan string]struct{}{}
	}
	progSubs[id][ch] = struct{}{}
	init := progVals[id]
	progMu.Unlock()

	// send initial
	fmt.Fprintf(c.Writer, "data: {\"percent\": %d}\n\n", init)
	c.Writer.Flush()

	notify := c.Request.Context().Done()
	for {
		select {
		case <-notify:
			progMu.Lock()
			delete(progSubs[id], ch)
			progMu.Unlock()
			return
		case msg := <-ch:
			io.WriteString(c.Writer, msg)
			c.Writer.Flush()
		}
	}
}

// --- HLS Encoding ---

func handleEncodeHLS(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var src string
	if err := dbpkg.DB.QueryRow("SELECT video_url FROM videos WHERE id=?", id).Scan(&src); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		return
	}

    outDir := filepath.Join("./hls", fmt.Sprint(id))
    ensureDir(outDir)

    go func() {
        progSetPercent(id, 0)
        if err := encodeToHLS(src, outDir, id, func(p int) { progSetPercent(id, p) }); err != nil {
            log.Println("encode failed:", err)
            return
        }
        progSetPercent(id, 100)
    }()
    c.JSON(http.StatusAccepted, gin.H{"status": "started"})
}

func encodeToHLS(input string, outDir string, videoID int64, onProgress func(int)) error {
    // Resolve local path if pointing to our own uploads
    in := resolveInputPath(input)
    durSec, _ := ffprobeDuration(in)

    renditions := []struct {
        Name     string
        Width    int
        Height   int
        VBitrate string
        ABitrate string
    }{
        {"360p", 640, 360, "800k", "96k"},
        {"480p", 854, 480, "1400k", "128k"},
        {"720p", 1280, 720, "2800k", "128k"},
        {"1080p", 1920, 1080, "5000k", "192k"},
    }

    for i, r := range renditions {
        variant := filepath.Join(outDir, r.Name+".m3u8")
        segPattern := filepath.Join(outDir, r.Name+"_%03d.ts")

        // Force CPU encoding (libx264) and use all CPU cores
        codec := "libx264"
        encExtra := []string{"-profile:v", "main", "-crf", "20", "-sc_threshold", "0", "-preset", "veryfast", "-threads", "0"}

        args := []string{
            "-y",
            "-i", in,
            // Preserve aspect, then force even dims to avoid encoder failures
            "-vf", fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease:eval=frame,scale=trunc(iw/2)*2:trunc(ih/2)*2", r.Width, r.Height),
            "-c:v", codec,
        }
        args = append(args, encExtra...)
        args = append(args,
            "-pix_fmt", "yuv420p",
            "-g", "48", "-keyint_min", "48",
            "-b:v", r.VBitrate,
            "-c:a", "aac", "-ar", "48000", "-b:a", r.ABitrate,
            "-hls_time", "6", "-hls_playlist_type", "vod", "-hls_flags", "independent_segments",
            "-hls_segment_filename", segPattern,
            variant,
        )
        log.Printf("starting ffmpeg for %s: %v", r.Name, args)
        if err := runFFmpegWithProgress(args, durSec, i, len(renditions), onProgress); err != nil {
            return err
        }

        // After each rendition, (re)write master with available variants
        if err := writeMaster(outDir); err != nil {
            log.Println("failed to write master:", err)
        }

        // After first rendition (360p) is ready, update DB to master so playback can start
        if i == 0 {
            masterURL := fmt.Sprintf("/hls/%d/master.m3u8", videoID)
            if base := publicBase(); base != "" {
                masterURL = base + masterURL
            }
            if _, err := dbpkg.DB.Exec("UPDATE videos SET video_url=?, encode_hls=1 WHERE id=?", masterURL, videoID); err != nil {
                log.Println("failed to update video url after first rendition:", err)
            }
        }
    }
    return nil
}

func writeMaster(outDir string) error {
	// Determine which variant playlists exist and write master accordingly
	type ent struct {
		bw   int
		res  string
		file string
	}
	candidates := []ent{
		{800000, "640x360", "360p.m3u8"},
		{1400000, "854x480", "480p.m3u8"},
		{2800000, "1280x720", "720p.m3u8"},
		{5000000, "1920x1080", "1080p.m3u8"},
	}
	lines := []string{"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-INDEPENDENT-SEGMENTS"}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(outDir, c.file)); err == nil {
			lines = append(lines, fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s", c.bw, c.res))
			lines = append(lines, c.file)
		}
	}
	master := filepath.Join(outDir, "master.m3u8")
	f, err := os.Create(master)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func resolveInputPath(u string) string {
	// Map our own served uploads to local paths for faster processing
	// e.g., http://localhost:8080/uploads/file.mp4 -> ./uploads/file.mp4
	if strings.HasPrefix(u, "/uploads/") {
		return "." + u
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		if parsed, err := neturl.Parse(u); err == nil {
			if strings.HasPrefix(parsed.Path, "/uploads/") {
				return "." + parsed.Path
			}
		}
	}
	// Otherwise let ffmpeg read from the given URL/path
	return u
}

func ensureDir(dir string) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal("failed to create dir:", err)
		}
	}
}

func loadEnvFile(filename string) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		os.Setenv(parts[0], parts[1])
	}
}

func main() {
    // load .env if present
    loadEnvFile(".env")
    // Connect DB
    dbpkg.Connect()
	ensureSchema(dbpkg.DB)

	// Gin
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())
    // Allow large uploads: configurable via env UPLOAD_MAX_MEMORY_MB (default 8192 MB = 8 GiB)
    maxMemMB := int64(8192)
    if v := os.Getenv("UPLOAD_MAX_MEMORY_MB"); v != "" {
        if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
            maxMemMB = n
        }
    }
    r.MaxMultipartMemory = maxMemMB << 20

	// Static serve uploaded files and HLS outputs
	ensureDir("./uploads")
	r.Static("/uploads", "./uploads")
	ensureDir("./hls")
	r.Static("/hls", "./hls")

	api := r.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		api.GET("/videos", handleListVideos)
		api.POST("/videos", handleCreateVideo)
		api.GET("/videos/:id/link", handleGetSignedLink)
		api.POST("/videos/:id/encode", handleEncodeHLS)
		api.GET("/videos/:id/encode/progress", handleEncodeProgressSSE)
		api.POST("/upload", handleUpload)
		api.GET("/upload/progress/:id", handleUploadProgressSSE)
		api.DELETE("/videos/:id", handleDeleteVideo)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// signed media route
	r.GET("/media/:id", handleMedia)
	r.HEAD("/media/:id", handleMedia)

	log.Println("API listening on :" + port)
	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           r,
		ReadHeaderTimeout: 15 * time.Second,
		// Allow very long reads for large uploads
		ReadTimeout:       2 * time.Hour,
		// Writes are small for API responses; keep generous for HLS proxying
		WriteTimeout:      2 * time.Hour,
		IdleTimeout:       2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		// Allow Vite dev server and any origin by default if not provided
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func ensureSchema(db *sql.DB) {
	const createVideos = `
CREATE TABLE IF NOT EXISTS videos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    nama_video TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    video_url TEXT NOT NULL,
    encode_hls INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`
	if _, err := db.Exec(createVideos); err != nil {
		log.Fatal("failed to ensure schema: ", err)
	}
}

func handleListVideos(c *gin.Context) {
	rows, err := dbpkg.DB.Query(`SELECT id, nama_video, slug, video_url, encode_hls, created_at FROM videos ORDER BY id DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var list []Video
	for rows.Next() {
		var v Video
		var enc int
		var created string
		if err := rows.Scan(&v.ID, &v.NamaVideo, &v.Slug, &v.VideoURL, &enc, &created); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		v.EncodeHLS = enc == 1
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			v.CreatedAt = t
		} else {
			// SQLite default CURRENT_TIMESTAMP returns "YYYY-MM-DD HH:MM:SS"
			if t2, e2 := time.Parse("2006-01-02 15:04:05", created); e2 == nil {
				v.CreatedAt = t2
			}
		}
		list = append(list, v)
	}

	c.JSON(http.StatusOK, list)
}

type createVideoReq struct {
	NamaVideo string `json:"nama_video" binding:"required"`
	Slug      string `json:"slug" binding:"required"`
	VideoURL  string `json:"video_url" binding:"required"`
	EncodeHLS bool   `json:"encode_hls"`
}

func handleCreateVideo(c *gin.Context) {
	var req createVideoReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Insert
	res, err := dbpkg.DB.Exec(`INSERT INTO videos (nama_video, slug, video_url, encode_hls) VALUES (?, ?, ?, ?)`, req.NamaVideo, req.Slug, req.VideoURL, boolToInt(req.EncodeHLS))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func getSignSecret() string {
	if s := os.Getenv("SIGN_SECRET"); s != "" {
		return s
	}
	return "dev-secret-change-me"
}

func makeSignature(parts ...string) string {
	h := hmac.New(sha256.New, []byte(getSignSecret()))
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte("|"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func handleGetSignedLink(c *gin.Context) {
	id := c.Param("id")
	ttlStr := c.Query("ttl")
	if ttlStr == "" {
		ttlStr = "3600" // default 1h
	}
	ttl, err := strconv.ParseInt(ttlStr, 10, 64)
	if err != nil || ttl <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ttl"})
		return
	}
	// fetch video metadata
	var v Video
	var enc int
	var created string
	if err := dbpkg.DB.QueryRow("SELECT id, nama_video, slug, video_url, encode_hls, created_at FROM videos WHERE id=?", id).
		Scan(&v.ID, &v.NamaVideo, &v.Slug, &v.VideoURL, &enc, &created); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video not found"})
		return
	}
	v.EncodeHLS = enc == 1
	if t, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
		v.CreatedAt = t
	}

	exp := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	sig := makeSignature(id, fmt.Sprint(exp))
	// Prefer explicit PUBLIC_BASE_URL for absolute links; else derive from request (respecting proxies)
	base := publicBase()
	if base == "" {
		scheme := c.GetHeader("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "http"
			if c.Request.TLS != nil {
				scheme = "https"
			}
		}
		base = fmt.Sprintf("%s://%s", scheme, c.Request.Host)
	}
	url := fmt.Sprintf("%s/media/%s?exp=%d&sig=%s", strings.TrimRight(base, "/"), id, exp, sig)
	c.JSON(http.StatusOK, gin.H{
		"url": url,
		"exp": exp,
		"video": gin.H{
			"id":         v.ID,
			"nama_video": v.NamaVideo,
			"slug":       v.Slug,
			"video_url":  v.VideoURL,
			"encode_hls": v.EncodeHLS,
		},
	})
}

func handleMedia(c *gin.Context) {
	id := c.Param("id")
	expStr := c.Query("exp")
	sig := c.Query("sig")
	if id == "" || expStr == "" || sig == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	expected := makeSignature(id, fmt.Sprint(exp))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	// lookup video url
	var url string
	if err := dbpkg.DB.QueryRow("SELECT video_url FROM videos WHERE id=?", id).Scan(&url); err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// Proxy upstream instead of redirecting
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		// forward Range header for seeking support if present
		if rng := c.GetHeader("Range"); rng != "" {
			req.Header.Set("Range", rng)
		}
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// propagate important headers
		for k, vv := range resp.Header {
			for _, v := range vv {
				c.Writer.Header().Add(k, v)
			}
		}
		c.Status(resp.StatusCode)
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			// best effort; connection might close by client
			return
		}
		return
	}
	// treat as local file path or /uploads path
	// normalize to ./ if begins with /
	local := url
	if strings.HasPrefix(local, "/") {
		local = "." + local
	}
	// if it's just a filename, assume uploads dir
	if !strings.Contains(local, "/") {
		local = filepath.Join("./uploads", local)
	}
	ext := strings.ToLower(filepath.Ext(local))
	// HLS playlist rewrite: make entries absolute to /hls/<id>/...
	if ext == ".m3u8" {
		c.Writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		// Compute web prefix, e.g. local="./hls/3/master.m3u8" => "/hls/3/"
		dir := filepath.Dir(local)
		// convert local dir like "./hls/3" -> web "/hls/3"
		webPrefix := "/" + strings.TrimPrefix(dir, "./") + "/"
		// read and rewrite
		b, err := os.ReadFile(local)
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		lines := strings.Split(string(b), "\n")
		for i, ln := range lines {
			s := strings.TrimSpace(ln)
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			// if already absolute or full URL, skip
			if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				continue
			}
			// rewrite to absolute under /hls/<id>/
			lines[i] = webPrefix + s
		}
		c.String(http.StatusOK, strings.Join(lines, "\n"))
		return
	}
	if ext == ".ts" {
		c.Writer.Header().Set("Content-Type", "video/mp2t")
	}
	c.File(local)
}

func handleUpload(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file is required"})
		return
	}

	// upload id for progress (from query id or generate)
	upID := c.Query("id")
	if upID == "" {
		upID = fmt.Sprintf("up-%d", time.Now().UnixNano())
	}
	upSetPercent(upID, 0)

	ensureDir("./uploads")
	filename := filepath.Base(fh.Filename)
	dest := filepath.Join("./uploads", filename)

	src, err := fh.Open()
	if err != nil {
		log.Printf("upload error open src id=%s name=%s err=%v", upID, filename, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer src.Close()

	out, err := os.Create(dest)
	if err != nil {
		log.Printf("upload error create dest id=%s name=%s err=%v", upID, filename, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer out.Close()

	log.Printf("upload start id=%s name=%s size=%d content_length=%s", upID, filename, fh.Size, c.Request.Header.Get("Content-Length"))
	var copied int64
	total := fh.Size
	lastPctLogged := -1
	buf := make([]byte, 1024*256) // 256KB
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				log.Printf("upload error write id=%s name=%s err=%v", upID, filename, werr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": werr.Error()})
				return
			}
			copied += int64(n)
			if total > 0 {
				pct := int((float64(copied) / float64(total)) * 100.0)
				if pct < 1 { pct = 1 }
				if pct > 99 { pct = 99 }
				upSetPercent(upID, pct)
				if pct != lastPctLogged && (pct%5 == 0 || pct == 1 || pct == 99) {
					log.Printf("upload progress id=%s name=%s copied=%d/%d pct=%d", upID, filename, copied, total, pct)
					lastPctLogged = pct
				}
			}
		}
		if rerr == io.EOF {
			log.Printf("upload complete id=%s name=%s size=%d", upID, filename, copied)
			break
		}
		if rerr != nil {
			log.Printf("upload error read id=%s name=%s err=%v", upID, filename, rerr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": rerr.Error()})
			return
		}
	}

	// build URL for DB: prefer PUBLIC_BASE_URL; if empty, store relative path
	base := publicBase()
	var url string
	if base == "" {
		url = "/uploads/" + filename
	} else {
		url = fmt.Sprintf("%s/uploads/%s", strings.TrimRight(base, "/"), filename)
	}
	upSetPercent(upID, 100)
	c.JSON(http.StatusOK, gin.H{"url": url, "upload_id": upID})
}

// publicBase returns an absolute base like https://media.nanimeid.xyz when PUBLIC_BASE_URL is set
func publicBase() string {
    if v := os.Getenv("PUBLIC_BASE_URL"); v != "" {
        return strings.TrimRight(v, "/")
    }
    return ""
}
