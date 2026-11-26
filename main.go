package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/gofrs/flock"
)

//go:embed web/*
var assets embed.FS

const maxMetainfo = 16 << 20

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Поток:", err)
		showDesktopError(err)
		os.Exit(1)
	}
}

func run() error {
	config, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	state := flag.String("state", filepath.Join(config, "Potok"), "каталог настроек и очереди")
	downloads := flag.String("downloads", filepath.Join(home, "Downloads", "Potok"), "папка загрузок")
	port := flag.Int("port", 47685, "локальный порт интерфейса (0 — свободный порт)")
	noBrowser := flag.Bool("no-browser", false, "не открывать браузер автоматически")
	flag.Parse()

	dir, err := filepath.Abs(*downloads)
	if err != nil {
		return err
	}
	for _, path := range []string{*state, dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	lock := flock.New(filepath.Join(*state, "potok.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("Поток уже запущен с этой папкой настроек")
	}
	defer lock.Close()

	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return fmt.Errorf("не удалось открыть интерфейс: %w", err)
	}
	defer listener.Close()

	files := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: dir,
		TorrentDirMaker: func(base string, _ *metainfo.Info, hash metainfo.Hash) string {
			return filepath.Join(base, hash.HexString())
		},
		DisableMmap: true,
	})
	defer files.Close()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DefaultStorage = files
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.NoUpload = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	queue := &queue{client: client, storage: files, path: filepath.Join(*state, "queue.json"), dir: dir}
	if err := queue.load(); err != nil {
		return fmt.Errorf("не удалось прочитать очередь: %w", err)
	}
	stop := make(chan struct{}, 1)
	server := &http.Server{
		Handler:           routes(listener.Addr().String(), queue, stop),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       25 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				queue.sample()
			case <-ctx.Done():
				return
			}
		}
	}()
	address := "http://" + listener.Addr().String()
	fmt.Printf("Поток — %s\nПапка загрузок — %s\n", address, dir)
	closeDesktop, err := startDesktop(address, *noBrowser, stop)
	if err != nil {
		cancel()
		<-stopped
		server.Close()
		return err
	}
	defer closeDesktop()

	select {
	case <-ctx.Done():
	case <-stop:
	case err := <-finished:
		if !errors.Is(err, http.ErrServerClosed) {
			cancel()
			<-stopped
			return err
		}
	}
	shutdown, end := context.WithTimeout(context.Background(), 3*time.Second)
	defer end()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
	}
	cancel()
	<-stopped
	queue.sample()
	return nil
}

func requestStop(stop chan<- struct{}) {
	select {
	case stop <- struct{}{}:
	default:
	}
}

func routes(host string, q *queue, stop chan<- struct{}) http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /api/queue", func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, q.view())
	})
	mux.HandleFunc("POST /api/torrents/preview", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetainfo))
		if err != nil {
			sendError(w, 400, "Файл должен быть не больше 16 МБ.")
			return
		}
		files, err := inspectMetainfo(body)
		if err != nil {
			sendError(w, 400, err.Error())
			return
		}
		send(w, 200, files)
	})
	mux.HandleFunc("POST /api/torrents", func(w http.ResponseWriter, r *http.Request) {
		item, err := readNewTorrent(w, r)
		if err != nil {
			sendError(w, 400, err.Error())
			return
		}
		if err := q.add(item); err != nil {
			sendError(w, 400, err.Error())
			return
		}
		send(w, 201, struct{}{})
	})
	mux.HandleFunc("GET /api/torrents/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		files, err := q.files(r.PathValue("id"))
		if err != nil {
			sendError(w, 400, err.Error())
			return
		}
		send(w, 200, files)
	})
	mux.HandleFunc("PUT /api/torrents/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetainfo))
		var skipped []int
		if err != nil || json.Unmarshal(body, &skipped) != nil || skipped == nil {
			sendError(w, 400, "Некорректный список файлов.")
			return
		}
		if err := q.selectFiles(r.PathValue("id"), skipped); err != nil {
			sendError(w, 400, err.Error())
			return
		}
		send(w, 200, struct{}{})
	})
	mux.HandleFunc("POST /api/torrents/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := q.action(r.PathValue("id"), r.PathValue("action")); err != nil {
			sendError(w, 400, err.Error())
			return
		}
		send(w, 200, struct{}{})
	})
	mux.HandleFunc("POST /api/folder", func(w http.ResponseWriter, r *http.Request) {
		if err := open(q.dir, false); err != nil {
			sendError(w, 500, "Не удалось открыть папку: "+err.Error())
			return
		}
		send(w, 200, struct{}{})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, struct{}{})
		requestStop(stop)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Header.Get("X-Potok") != "1" || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != "http://"+host) {
				sendError(w, 403, "Доступ ограничен локальным интерфейсом.")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func readNewTorrent(w http.ResponseWriter, r *http.Request) (saved, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return saved{}, errors.New("загрузите .torrent или вставьте magnet-ссылку")
	}
	if contentType == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, 2*maxMetainfo+4096)
		if err := r.ParseMultipartForm(2 * maxMetainfo); err != nil {
			return saved{}, errors.New("не удалось прочитать файл и список")
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("torrent")
		if err != nil {
			return saved{}, errors.New("выберите файл .torrent")
		}
		defer file.Close()
		body, err := io.ReadAll(io.LimitReader(file, maxMetainfo+1))
		if err != nil || len(body) > maxMetainfo {
			return saved{}, errors.New("файл должен быть не больше 16 МБ")
		}
		var skipped []int
		if json.Unmarshal([]byte(r.FormValue("skipped")), &skipped) != nil || skipped == nil {
			return saved{}, errors.New("некорректный список файлов")
		}
		return saved{Metadata: body, Skipped: skipped}, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetainfo))
	if err != nil {
		return saved{}, errors.New("файл должен быть не больше 16 МБ")
	}
	switch contentType {
	case "application/x-bittorrent":
		if len(body) == 0 {
			return saved{}, errors.New("выберите файл .torrent")
		}
		return saved{Metadata: body}, nil
	case "text/plain":
		magnet := strings.TrimSpace(string(body))
		if !strings.HasPrefix(strings.ToLower(magnet), "magnet:?") || len(magnet) > 64000 {
			return saved{}, errors.New("Вставьте корректную magnet-ссылку.")
		}
		return saved{Magnet: magnet}, nil
	default:
		return saved{}, errors.New("загрузите .torrent или вставьте magnet-ссылку")
	}
}

func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func sendError(w http.ResponseWriter, status int, message string) {
	send(w, status, map[string]string{"error": message})
}
