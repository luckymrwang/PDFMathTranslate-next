package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// previewWidth is the pixel width of the first-page result preview.
const previewWidth = 720

func (s *Server) previewPath(orderID string) string {
	return filepath.Join(s.pay.store.dir, "previews", orderID+".jpg")
}

// writePreview serves a JPEG of the first translated page, rendered once per order
// and removed together with the order's files when they expire.
func (s *Server) writePreview(w http.ResponseWriter, r *http.Request, order PayOrder) {
	path := s.previewPath(order.ID)
	if _, err := os.Stat(path); err != nil {
		if err := s.renderPreview(r.Context(), order, path); err != nil {
			log.Printf("result preview failed (order=%s): %v", order.ID, err)
			writeError(w, http.StatusBadGateway, "预览暂不可用")
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func (s *Server) renderPreview(ctx context.Context, order PayOrder, dest string) error {
	// The mono PDF holds only translated pages; the dual one is the fallback.
	source, key := order.MonoFile, order.MonoKey
	if source == "" && key == "" {
		source, key = order.DualFile, order.DualKey
	}
	if source == "" && key != "" && s.storage != nil {
		resp, err := s.storage.GetObject(ctx, key)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		tmp, err := os.CreateTemp("", "preview-*.pdf")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, err = io.Copy(tmp, resp.Body)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		source = tmp.Name()
	}
	if source == "" {
		return errors.New("no translated PDF")
	}
	image, err := s.uploadFile(ctx, "/v1/pdf/render", source, order.ID+".pdf",
		map[string]any{"page": 1, "width": previewWidth}, "")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	// Rename keeps concurrent first requests from serving a half-written file.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".preview-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(image)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
