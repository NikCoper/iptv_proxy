/*
 * Iptv-Proxy is a project to proxyfie an m3u file and to proxyfie an Xtream iptv service (client API).
 * Copyright (C) 2020  Pierre-Emmanuel Jacquier
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/grafov/m3u8"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed fake.ts
var fakeTS []byte
var (
	currentProcess   *FFmpegProcess
	lastRequestTimer *time.Timer
	timerMutex       sync.Mutex
)

type FFmpegProcess struct {
	Cmd      *exec.Cmd
	LastPath string
}

func (c *Config) getM3U(ctx *gin.Context) {
	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	ctx.Header("Content-Type", "application/octet-stream")

	ctx.File(c.proxyfiedM3UPath)
}

func (c *Config) reverseProxy(ctx *gin.Context) {
	rpURL, err := url.Parse(c.track.URI)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	c.stream(ctx, rpURL)
}
func (c *Config) tsHandler(ctx *gin.Context) {
	log.Println("tsHandler called")
	streamID := ctx.Param("streamID")
	tsID := ctx.Param("tsID")
	// Путь к каталогу hlsdownloads
	filePath := fmt.Sprintf("hlsdownloads/%s/stream/%s", tsID, streamID)

	// Проверка существования файла
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// Если файла нет, отдаем фейковый файл
		ctx.Data(http.StatusOK, "video/MP2T", fakeTS)
		return
	}

	// Отдаем реальный файл
	ctx.File(filePath)
}

func (c *Config) m3u8ReverseProxy(ctx *gin.Context) {
	timerMutex.Lock()
	if lastRequestTimer != nil {
		lastRequestTimer.Stop()
	}
	lastRequestTimer = time.AfterFunc(1*time.Minute, func() {
		if currentProcess != nil {
			err := currentProcess.Cmd.Process.Kill()
			if err != nil {
				return
			}
			_, _ = currentProcess.Cmd.Process.Wait()
			removeDirectoryFromPath(currentProcess.LastPath)
			currentProcess = nil
		}
	})
	timerMutex.Unlock()
	id := ctx.Param("id")
	var idStream string

	rpURL, err := url.Parse(strings.ReplaceAll(c.track.URI, path.Base(c.track.URI), id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	fullURL := rpURL.Scheme + "://" + rpURL.Host + rpURL.Path
	parts := strings.Split(rpURL.Path, "/")
	if len(parts) > 2 {
		idStream = parts[len(parts)-2] // предпоследний элемент
	} else {
		idStream = "0"
	}

	// Создание каталога, если он не существует
	dirPath := fmt.Sprintf("hlsdownloads/%s/stream", idStream)
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		err = os.MkdirAll(dirPath, 0755)
		if err != nil {
			// Обработка ошибки
			log.Fatal(err)
		}
	}

	outputPath := fmt.Sprintf("%s/stream.m3u8", dirPath)

	if currentProcess != nil {
		if currentProcess.LastPath == rpURL.Path {
			// Если путь не изменился, просто отдаем файл
			ModifyAndSendPlaylist(ctx, outputPath)
			return
		} else {
			// Если путь изменился, завершаем текущий процесс
			if err := currentProcess.Cmd.Process.Kill(); err != nil {
				// Обработка ошибки, например, запись в лог
				log.Println("Failed to kill process:", err)
			}
			if _, err := currentProcess.Cmd.Process.Wait(); err != nil {
				// Обработка ошибки, например, запись в лог
				log.Println("Failed to wait for process:", err)
			}

			removeDirectoryFromPath(currentProcess.LastPath)
			currentProcess = nil
		}
	}

	resp, err := http.Get(fullURL)
	if err != nil {
		log.Fatal(err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	p, listType, err := m3u8.DecodeFrom(bufio.NewReader(resp.Body), true)
	if err != nil {
		log.Fatal(err)
	}

	var hlsTime, hlsListSize string

	if listType == m3u8.MEDIA {
		mediaList := p.(*m3u8.MediaPlaylist)
		hlsTime = fmt.Sprintf("%.0f", mediaList.TargetDuration)
		var count int
		for _, segment := range mediaList.Segments {
			if segment != nil {
				count++
			}
		}
		hlsListSize = fmt.Sprintf("%d", count)
	}

	// Считываем переменные окружения
	bitrateVideo := os.Getenv("BITRATE_VIDEO")
	if bitrateVideo == "" {
		bitrateVideo = "600k" // значение по умолчанию
	}

	bitrateAudio := os.Getenv("BITRATE_AUDIO")
	if bitrateAudio == "" {
		bitrateAudio = "128k" // значение по умолчанию
	}

	scale := os.Getenv("SCALE")
	if scale == "" {
		scale = "1280:720" // значение по умолчанию
	}

	crf := os.Getenv("CRF")
	if crf == "" {
		crf = "32" // значение по умолчанию
	}

	preset := os.Getenv("PRESET")
	if preset == "" {
		preset = "ultrafast" // значение по умолчанию
	}

	fmt.Println("CRF:", crf)
	fmt.Println("SCALE:", scale)
	fmt.Println("BITRATE_VIDEO:", bitrateVideo)
	fmt.Println("BITRATE_AUDIO:", bitrateAudio)
	fmt.Println("HLS_TIME:", hlsTime)
	fmt.Println("HLS_LIST_SIZE:", hlsListSize)
	// Запуск ffmpeg для трансляции
	cmd := exec.Command("ffmpeg", "-i", fullURL,
		"-c:v", "libx265", "-preset", preset, "-tune", "zerolatency", "-crf", crf,
		"-vf", "scale="+scale,
		"-b:v", bitrateVideo,
		"-c:a", "aac", "-b:a", bitrateAudio,
		"-hls_time", hlsTime,
		"-hls_list_size", hlsListSize,
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", dirPath+"/data%02d.ts", // Сегменты сохраняются в папке stream
		"-hls_flags", "independent_segments+delete_segments",
		outputPath)
	cmd.Stdout = os.Stdout // Перенаправляем стандартный вывод
	cmd.Stderr = os.Stderr // Перенаправляем стандартный вывод ошибок

	err = cmd.Start()
	if err != nil {
		log.Fatal(err)
	}

	maxAttempts := 60
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Проверка существования файла
		if _, err := os.Stat(outputPath); os.IsNotExist(err) {
			// Если файла нет и это не последняя попытка, ждем и пробуем снова
			if attempt < maxAttempts {
				time.Sleep(1 * time.Second)
				continue
			}
		} else {
			// Если файл существует, выходим из цикла
			break
		}
	}
	// Сохраняем информацию о текущем процессе
	currentProcess = &FFmpegProcess{
		Cmd:      cmd,
		LastPath: rpURL.Path,
	}
	ModifyAndSendPlaylist(ctx, outputPath)
}

func removeDirectoryFromPath(path string) {
	// Регулярное выражение для извлечения числа из строки
	re := regexp.MustCompile(`/(\d+)/`)
	matches := re.FindStringSubmatch(path)

	if len(matches) > 1 {
		id := matches[1]
		dirPath := "hlsdownloads/" + id

		// Удаление каталога
		if err := os.RemoveAll(dirPath); err != nil {
			// Обработка ошибки, например, запись в лог
			log.Println("Failed to remove directory:", err)
		}
	}
}

func ModifyAndSendPlaylist(ctx *gin.Context, outputPath string) {
	// Откройте файл для чтения
	file, err := os.Open(outputPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func(file *os.File) {
		_ = file.Close()
	}(file)

	// Декодируйте содержимое файла
	p, listType, err := m3u8.DecodeFrom(bufio.NewReader(file), true)
	if err != nil {
		log.Fatal(err)
	}

	if listType == m3u8.MEDIA {
		mediaList := p.(*m3u8.MediaPlaylist)

		// Добавьте префикс "stream/" к URI каждого сегмента
		for _, segment := range mediaList.Segments {
			if segment != nil {
				segment.URI = "/" + filepath.Dir(outputPath) + "/" + segment.URI

			}
		}

		// Генерируйте новый плейлист
		modifiedPlaylist := mediaList.Encode().Bytes()

		// Отправьте новый плейлист пользователю
		ctx.Data(http.StatusOK, "application/vnd.apple.mpegurl", modifiedPlaylist)
	}
}

func (c *Config) stream(ctx *gin.Context, oriURL *url.URL) {
	client := &http.Client{}

	req, err := http.NewRequest("GET", oriURL.String(), nil)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	mergeHttpHeader(req.Header, ctx.Request.Header)

	resp, err := client.Do(req)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	mergeHttpHeader(ctx.Writer.Header(), resp.Header)
	ctx.Status(resp.StatusCode)
	ctx.Stream(func(w io.Writer) bool {
		_, _ = io.Copy(w, resp.Body) // nolint: errcheck
		return false
	})
}

func (c *Config) xtreamStream(ctx *gin.Context, oriURL *url.URL) {
	id := ctx.Param("id")
	if strings.HasSuffix(id, ".m3u8") {
		c.hlsXtreamStream(ctx, oriURL)
		return
	}

	c.stream(ctx, oriURL)
}

type values []string

func (vs values) contains(s string) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}

	return false
}

func mergeHttpHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			if values(dst.Values(k)).contains(v) {
				continue
			}
			dst.Add(k, v)
		}
	}
}

// authRequest handle auth credentials
type authRequest struct {
	Username string `form:"username" binding:"required"`
	Password string `form:"password" binding:"required"`
}

func (c *Config) authenticate(ctx *gin.Context) {
	var authReq authRequest
	if err := ctx.Bind(&authReq); err != nil {
		_ = ctx.AbortWithError(http.StatusBadRequest, err) // nolint: errcheck
		return
	}
	if c.ProxyConfig.User.String() != authReq.Username || c.ProxyConfig.Password.String() != authReq.Password {
		ctx.AbortWithStatus(http.StatusUnauthorized)
	}
}

func (c *Config) appAuthenticate(ctx *gin.Context) {
	contents, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	q, err := url.ParseQuery(string(contents))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	if len(q["username"]) == 0 || len(q["password"]) == 0 {
		_ = ctx.AbortWithError(http.StatusBadRequest, fmt.Errorf("bad body url query parameters")) // nolint: errcheck
		return
	}
	log.Printf("[iptv-proxy] %v | %s |App Auth\n", time.Now().Format("2006/01/02 - 15:04:05"), ctx.ClientIP())
	if c.ProxyConfig.User.String() != q["username"][0] || c.ProxyConfig.Password.String() != q["password"][0] {
		ctx.AbortWithStatus(http.StatusUnauthorized)
	}

	ctx.Request.Body = io.NopCloser(bytes.NewReader(contents))
}
