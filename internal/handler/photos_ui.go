package handler

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

func (h *DashboardHandler) photos(w http.ResponseWriter, r *http.Request) {
	if h.optionalUser(r) == nil {
		http.Redirect(w, r, "/auth/login?to="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	page, err := staticFiles.ReadFile("static/photos.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Write(page)
}
func registerPhotoCodecs(mux *http.ServeMux) {
	mux.Handle("GET /photo-codecs/v3/{file}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := r.PathValue("file")
		asset := file
		if file == "wasm.js" {
			asset = "libheif.wasm"
		}
		b, err := staticFiles.ReadFile("static/photo-codecs/v3/" + asset)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if file == "wasm.js" {
			b = []byte("self.photoWasm='" + base64.StdEncoding.EncodeToString(b) + "';")
		}
		kind := "text/plain; charset=utf-8"
		if strings.HasSuffix(file, ".js") {
			kind = "text/javascript; charset=utf-8"
		}
		if strings.HasSuffix(file, ".wasm") {
			kind = "application/wasm"
		}
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self'")
		w.Write(b)
	}))
}
