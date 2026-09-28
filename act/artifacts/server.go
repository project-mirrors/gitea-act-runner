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
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/julienschmidt/httprouter"
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

func uploads(router *httprouter.Router, baseDir string) {
	router.POST("/_apis/pipelines/workflows/:runId/artifacts", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		runID := params.ByName("runId")

		writeJSON(w, FileContainerResourceURL{
			FileContainerResourceURL: fmt.Sprintf("http://%s/upload/%s", req.Host, runID),
		})
	})

	router.PUT("/upload/:runId", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		itemPath := req.URL.Query().Get("itemPath")
		runID := params.ByName("runId")

		if req.Header.Get("Content-Encoding") == "gzip" {
			itemPath += gzipExtension
		}

		safeRunPath := safeResolve(baseDir, runID)
		safePath := safeResolve(safeRunPath, itemPath)

		if err := os.MkdirAll(filepath.Dir(safePath), os.ModePerm); err != nil {
			panic(err)
		}
		flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		appendUpload := req.Header.Get("Content-Range")
		if appendUpload != "" && !strings.HasPrefix(appendUpload, "bytes 0-") {
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		file, err := os.OpenFile(safePath, flags, 0o644)
		if err != nil {
			panic(err)
		}
		defer file.Close()
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

	router.PATCH("/_apis/pipelines/workflows/:runId/artifacts", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		writeJSON(w, ResponseMessage{
			Message: "success",
		})
	})
}

func downloads(router *httprouter.Router, baseDir string) {
	router.GET("/_apis/pipelines/workflows/:runId/artifacts", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		runID := params.ByName("runId")

		safePath := safeResolve(baseDir, runID)

		entries, err := os.ReadDir(safePath)
		if err != nil {
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

	router.GET("/download/:container", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		container := params.ByName("container")
		itemPath := req.URL.Query().Get("itemPath")
		safePath := safeResolve(baseDir, filepath.Join(container, itemPath))

		var files []ContainerItem
		err := filepath.WalkDir(safePath, func(path string, entry fs.DirEntry, err error) error {
			if !entry.IsDir() {
				rel, err := filepath.Rel(safePath, path)
				if err != nil {
					panic(err)
				}

				// if it was upload as gzip
				rel = strings.TrimSuffix(rel, gzipExtension)
				path := filepath.Join(itemPath, rel)

				rel = filepath.ToSlash(rel)
				path = filepath.ToSlash(path)

				files = append(files, ContainerItem{
					Path:            path,
					ItemType:        "file",
					ContentLocation: fmt.Sprintf("http://%s/artifact/%s/%s/%s", req.Host, container, itemPath, rel),
				})
			}
			return nil
		})
		if err != nil {
			panic(err)
		}

		writeJSON(w, ContainerItemResponse{
			Value: files,
		})
	})

	router.GET("/artifact/*path", func(w http.ResponseWriter, req *http.Request, params httprouter.Params) {
		path := params.ByName("path")[1:]

		safePath := safeResolve(baseDir, path)

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

	router := httprouter.New()

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
			logger.Fatal(err)
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
