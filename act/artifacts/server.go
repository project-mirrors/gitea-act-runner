// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
)

type FileContainerResourceURL struct {
	FileContainerResourceURL string `json:"fileContainerResourceUrl"`
}

type NamedFileContainerResourceURL struct {
	Name                     string `json:"name"`
	FileContainerResourceURL string `json:"fileContainerResourceUrl"`
}

type NamedFileContainerResourceURLResponse struct {
	Count int                             `json:"count"`
	Value []NamedFileContainerResourceURL `json:"value"`
}

type ContainerItem struct {
	Path            string `json:"path"`
	ItemType        string `json:"itemType"`
	ContentLocation string `json:"contentLocation"`
}

type ContainerItemResponse struct {
	Value []ContainerItem `json:"value"`
}

type ResponseMessage struct {
	Message string `json:"message"`
}

var gzipExtension = ".gz__"

func safeResolve(baseDir, relPath string) string {
	return filepath.Join(baseDir, filepath.Clean(filepath.Join(string(os.PathSeparator), relPath)))
}

func writeJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
}

func uploads(router *http.ServeMux, baseDir string) {
	router.HandleFunc("POST /_apis/pipelines/workflows/{runId}/artifacts", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, FileContainerResourceURL{
			FileContainerResourceURL: fmt.Sprintf("http://%s/upload/%s", req.Host, req.PathValue("runId")),
		})
	})

	router.HandleFunc("PUT /upload/{runId}", func(w http.ResponseWriter, req *http.Request) {
		itemPath := req.URL.Query().Get("itemPath")

		if req.Header.Get("Content-Encoding") == "gzip" {
			itemPath += gzipExtension
		}

		safePath := safeResolve(safeResolve(baseDir, req.PathValue("runId")), itemPath)

		if err := os.MkdirAll(filepath.Dir(safePath), os.ModePerm); err != nil {
			panic(err)
		}
		var start int64
		if contentRange := req.Header.Get("Content-Range"); contentRange != "" {
			if _, err := fmt.Sscanf(contentRange, "bytes %d-", &start); err != nil || start < 0 {
				http.Error(w, "invalid content range", http.StatusBadRequest)
				return
			}
		}
		flags := os.O_CREATE | os.O_WRONLY
		if start == 0 {
			flags |= os.O_TRUNC
		}
		file, err := os.OpenFile(safePath, flags, 0o644)
		if err != nil {
			panic(err)
		}
		defer file.Close()
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			panic(err)
		}
		if req.Body == nil {
			panic(errors.New("no body given"))
		}

		_, err = io.Copy(file, req.Body)
		if err != nil {
			panic(err)
		}

		writeJSON(w, ResponseMessage{
			Message: "success",
		})
	})

	router.HandleFunc("PATCH /_apis/pipelines/workflows/{runId}/artifacts", func(w http.ResponseWriter, _req *http.Request) {
		writeJSON(w, ResponseMessage{
			Message: "success",
		})
	})
}

func downloads(router *http.ServeMux, baseDir string) {
	router.HandleFunc("GET /_apis/pipelines/workflows/{runId}/artifacts", func(w http.ResponseWriter, req *http.Request) {
		runID := req.PathValue("runId")

		entries, err := os.ReadDir(safeResolve(baseDir, runID))
		if err != nil && !os.IsNotExist(err) {
			panic(err)
		}

		var list []NamedFileContainerResourceURL
		for _, entry := range entries {
			list = append(list, NamedFileContainerResourceURL{
				Name:                     entry.Name(),
				FileContainerResourceURL: fmt.Sprintf("http://%s/download/%s", req.Host, runID),
			})
		}

		writeJSON(w, NamedFileContainerResourceURLResponse{
			Count: len(list),
			Value: list,
		})
	})

	router.HandleFunc("GET /download/{container}", func(w http.ResponseWriter, req *http.Request) {
		container := req.PathValue("container")
		itemPath := req.URL.Query().Get("itemPath")
		safePath := safeResolve(baseDir, filepath.Join(container, itemPath))

		var files []ContainerItem
		err := filepath.WalkDir(safePath, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				rel, err := filepath.Rel(safePath, path)
				if err != nil {
					panic(err)
				}

				// if it was upload as gzip
				rel = strings.TrimSuffix(rel, gzipExtension)

				files = append(files, ContainerItem{
					Path:            filepath.ToSlash(filepath.Join(itemPath, rel)),
					ItemType:        "file",
					ContentLocation: (&url.URL{Scheme: "http", Host: req.Host, Path: "/artifact/" + container + "/" + itemPath + "/" + filepath.ToSlash(rel)}).String(),
				})
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			panic(err)
		}

		writeJSON(w, ContainerItemResponse{
			Value: files,
		})
	})

	router.HandleFunc("GET /artifact/{path...}", func(w http.ResponseWriter, req *http.Request) {
		safePath := safeResolve(baseDir, req.PathValue("path"))

		file, err := os.Open(safePath)
		if err != nil {
			// try gzip file
			file, err = os.Open(safePath + gzipExtension)
			if err != nil {
				panic(err)
			}
			w.Header().Add("Content-Encoding", "gzip")
		}
		defer file.Close()

		_, err = io.Copy(w, file)
		if err != nil {
			panic(err)
		}
	})
}

func Serve(ctx context.Context, artifactPath, addr, port string) context.CancelFunc {
	serverContext, cancel := context.WithCancel(ctx)
	logger := common.Logger(serverContext)

	if artifactPath == "" {
		return cancel
	}

	router := http.NewServeMux()

	logger.Debugf("Artifacts base path '%s'", artifactPath)
	uploads(router, artifactPath)
	downloads(router, artifactPath)

	server := &http.Server{
		Addr:              net.JoinHostPort(addr, port),
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           router,
	}

	// run server
	go func() {
		logger.Infof("Start server on http://%s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err)
			os.Exit(1)
		}
	}()

	// wait for cancel to gracefully shutdown server
	go func() {
		<-serverContext.Done()

		if err := server.Shutdown(ctx); err != nil {
			logger.Errorf("Failed shutdown gracefully - force shutdown: %v", err)
			server.Close()
		}
	}()

	return cancel
}
