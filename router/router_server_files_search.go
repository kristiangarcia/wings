package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/gin-gonic/gin"

	"github.com/0x7d8/wings/router/middleware"
	"github.com/0x7d8/wings/server/filesystem"
)

func postServerSearchFiles(c *gin.Context) {
	s := ExtractServer(c)

	var data struct {
		RootPath       string `json:"root"`
		Query          string `json:"query"`
		IncludeContent bool   `json:"include_content"`
		Limit          int    `json:"limit,omitempty"`
		MaxSize        int64  `json:"max_size,omitempty"`
	}

	if err := c.BindJSON(&data); err != nil {
		return
	}

	if data.Query == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "A query parameter must be provided.",
		})
		return
	}

	if data.Limit <= 0 {
		data.Limit = 100
	}

	if data.MaxSize <= 0 {
		data.MaxSize = 1024 * 1024 // 1MB default
	}

	type StatResult struct {
		Name      string    `json:"name"`
		Created   time.Time `json:"created"`
		Modified  time.Time `json:"modified"`
		Mode      string    `json:"mode"`
		ModeBits  string    `json:"mode_bits"`
		Size      int64     `json:"size"`
		Directory bool      `json:"directory"`
		File      bool      `json:"file"`
		Symlink   bool      `json:"symlink"`
		Mime      string    `json:"mime"`
	}

	results := make([]StatResult, 0, min(50, data.Limit))
	resultsMux := sync.Mutex{}
	queryLower := strings.ToLower(data.Query)
	resultCount := atomic.Int32{}

	pending := make(chan string, 1000)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 8192)

			for path := range pending {
				if resultCount.Load() >= int32(data.Limit) {
					continue
				}

				info, err := s.Filesystem().UnixFS().Stat(path)
				if err != nil {
					continue
				}

				// Skip large files for content search
				if info.Size() > data.MaxSize {
					if strings.Contains(strings.ToLower(path), queryLower) {
						if stat, err := statFromPath(s.Filesystem(), path); err == nil {
							resultsMux.Lock()
							if len(results) < data.Limit {
								results = append(results, StatResult{
									Name:      strings.TrimPrefix(strings.TrimPrefix(path, data.RootPath), "/"),
									Created:   stat.CTime(),
									Modified:  stat.ModTime(),
									Mode:      stat.Mode().String(),
									ModeBits:  fmt.Sprintf("%o", stat.Mode().Perm()),
									Size:      stat.Size(),
									Directory: stat.IsDir(),
									File:      stat.Mode().IsRegular(),
									Symlink:   stat.Mode()&os.ModeSymlink != 0,
									Mime:      stat.Mimetype,
								})
								resultCount.Add(1)
							}
							resultsMux.Unlock()
						}
					}
					continue
				}

				if strings.Contains(strings.ToLower(path), queryLower) {
					if stat, err := statFromPath(s.Filesystem(), path); err == nil {
						resultsMux.Lock()
						if len(results) < data.Limit {
							results = append(results, StatResult{
								Name:      strings.TrimPrefix(strings.TrimPrefix(path, data.RootPath), "/"),
								Created:   stat.CTime(),
								Modified:  stat.ModTime(),
								Mode:      stat.Mode().String(),
								ModeBits:  fmt.Sprintf("%o", stat.Mode().Perm()),
								Size:      stat.Size(),
								Directory: stat.IsDir(),
								File:      stat.Mode().IsRegular(),
								Symlink:   stat.Mode()&os.ModeSymlink != 0,
								Mime:      stat.Mimetype,
							})
							resultCount.Add(1)
						}
						resultsMux.Unlock()
					}
					continue
				}

				if !data.IncludeContent {
					continue
				}

				file, err := s.Filesystem().UnixFS().Open(path)
				if err != nil {
					continue
				}

				n, err := file.Read(buf[:512])
				if err != nil || (n > 0 && bytes.Contains(buf[:n], []byte{0})) {
					file.Close()
					continue
				}

				// Reset to start of file after binary check
				if _, err := file.Seek(0, 0); err != nil {
					file.Close()
					continue
				}

				found := false
				var lastChunk []byte
				for !found {
					n, err := file.Read(buf)
					if n <= 0 {
						break
					}
					
					// Combine with previous chunk's remainder to handle split matches
					searchChunk := append(lastChunk, buf[:n]...)
					if strings.Contains(strings.ToLower(string(searchChunk)), queryLower) {
						if stat, err := statFromPath(s.Filesystem(), path); err == nil {
							resultsMux.Lock()
							if len(results) < data.Limit {
								results = append(results, StatResult{
									Name:      strings.TrimPrefix(strings.TrimPrefix(path, data.RootPath), "/"),
									Created:   stat.CTime(),
									Modified:  stat.ModTime(),
									Mode:      stat.Mode().String(),
									ModeBits:  fmt.Sprintf("%o", stat.Mode().Perm()),
									Size:      stat.Size(),
									Directory: stat.IsDir(),
									File:      stat.Mode().IsRegular(),
									Symlink:   stat.Mode()&os.ModeSymlink != 0,
									Mime:      stat.Mimetype,
								})
								resultCount.Add(1)
							}
							resultsMux.Unlock()
						}
						found = true
					}

					// Keep last portion that's the length of query for next chunk
					if n >= len(queryLower) {
						lastChunk = buf[n-len(queryLower):n]
					}
					
					if err == io.EOF || int64(len(searchChunk)) > data.MaxSize {
						break
					}
				}
				file.Close()
			}
		}()
	}

	err := s.Filesystem().UnixFS().WalkDir(data.RootPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if resultCount.Load() >= int32(data.Limit) {
			return io.EOF
		}
		pending <- path
		return nil
	})

	close(pending)
	wg.Wait()

	if err != nil && err != io.EOF {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Sort results
	slices.SortStableFunc(results, func(a, b StatResult) int {
		switch {
		case a.Name == b.Name:
			return 0
		case a.Name > b.Name:
			return 1
		default:
			return -1
		}
	})

	slices.SortStableFunc(results, func(a, b StatResult) int {
		switch {
		case a.Directory && b.Directory:
			return 0
		case a.Directory:
			return -1
		default:
			return 1
		}
	})

	c.JSON(http.StatusOK, gin.H{
		"results": results,
	})
}

func statFromPath(fs *filesystem.Filesystem, path string) (filesystem.Stat, error) {
	info, err := fs.UnixFS().Stat(path)
	if err != nil {
		return filesystem.Stat{}, err
	}

	var mt string
	if info.IsDir() {
		mt = "inode/directory"
	} else {
		mt = "application/octet-stream"
		if info.Mode().IsRegular() {
			file, err := fs.UnixFS().Open(path)
			if err != nil {
				return filesystem.Stat{}, err
			}
			m, err := mimetype.DetectReader(file)
			if err == nil {
				mt = m.String()
			}
			file.Close()
		}
	}

	return filesystem.Stat{FileInfo: info, Mimetype: mt}, nil
}