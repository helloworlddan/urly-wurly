// Package main builds a basic HTTP server to provide URL shortening functions on GCP.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"cloud.google.com/go/profiler"
	"cloud.google.com/go/storage"

	"github.com/gorilla/mux"
	"github.com/mr-tron/base58"

	"contrib.go.opencensus.io/exporter/stackdriver"
	"go.opencensus.io/trace"
)

// struct response forms a JSON response for the servers API.
type response struct {
	// Shortened URL (if successful)
	ShortenedURL string `json:"shortened_url,omitempty"`
	// Informative message about what has happened
	Message string `json:"message"`
}

// Launch HTTP server, register routes & handlers and server static files
func main() {
	err := profiler.Start(profiler.Config{
		Service:              "urly-wurly",
		NoHeapProfiling:      true,
		NoAllocProfiling:     true,
		NoGoroutineProfiling: true,
		DebugLogging:         true,
		ServiceVersion:       "1.0.0",
	})
	if err != nil {
		log.Fatal(err)
	}
	exporter, err := stackdriver.NewExporter(stackdriver.Options{})
	if err != nil {
		log.Fatal(err)
	}
	trace.RegisterExporter(exporter)
	exporter.StartMetricsExporter()
	defer exporter.StopMetricsExporter()
	trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})

	ctx := context.Background()
	ctx, span := trace.StartSpan(ctx, "main")
	defer span.End()

	router := mux.NewRouter()
	router.HandleFunc("/s", shortenHandler).Methods(http.MethodGet, http.MethodPost, http.MethodOptions)
	router.HandleFunc("/{id:[\\w-]+}", lengthenHandler).Methods(http.MethodGet, http.MethodOptions)
	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./public/")))
	router.Use(mux.CORSMethodMiddleware(router))
	http.Handle("/", router)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil))
}

// GET & POST handler to shorten URLs
func shortenHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	ctx, span := trace.StartSpan(ctx, "shortenHandler")
	defer span.End()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodOptions {
		return
	}
	parameters, ok := r.URL.Query()["url"]
	if !ok || len(parameters[0]) < 1 {
		parameters, ok = r.URL.Query()["text"]
		if !ok || len(parameters[0]) < 1 {
			respond(ctx, response{"", "no url to shorten provided!"}, http.StatusBadRequest, w)
			return
		}
	}
	encodedLongURL := strings.TrimSpace(parameters[0])
	longURL, err := url.QueryUnescape(encodedLongURL)
	if err != nil {
		respond(ctx, response{"", "unable to decode URL. was it encoded?"}, http.StatusBadRequest, w)
		return
	}
	uri, err := url.Parse(longURL)
	if err != nil {
		respond(ctx, response{"", "unable to parse URI. was it encoded?"}, http.StatusBadRequest, w)
		return
	}
	if uri.Scheme != "https" && uri.Scheme != "http" {
		respond(ctx, response{"", "provided input is not a HTTP/HTTPS URL!"}, http.StatusBadRequest, w)
		return
	}

	custom := ""
	parameters, ok = r.URL.Query()["customname"]
	if ok {
		custom = parameters[0]
		reg, err := regexp.Compile(`^[\w-]{6,}$`)
		if err != nil {
			respond(ctx, response{"", "unable to compile regex"}, http.StatusInternalServerError, w)
		}

		longURL, err := gcsRead(ctx, custom)
		if longURL != "" {
			respond(ctx, response{"", "Custom name already registered to another URL!"}, http.StatusBadRequest, w)
			return
		}

		if !reg.MatchString(custom) {
			respond(ctx, response{"", "custom name should be at least 6 alphanumeric characters incl. underscores and dashes!"}, http.StatusBadRequest, w)
			return
		}
	}

	shortURL, err := shortenURL(ctx, longURL, custom)
	if err != nil {
		respond(ctx, response{"", "unable to access GCS!"}, http.StatusInternalServerError, w)
		return
	}
	respond(ctx, response{shortURL, "url shortened!"}, http.StatusOK, w)
}

// GET handler to lengthen a previously shortened URLS.
// Upon success, HTTP 302 will be returned to redirect to long URL
func lengthenHandler(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	ctx, span := trace.StartSpan(ctx, "lengthenHandler")
	defer span.End()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		return
	}
	short := mux.Vars(r)["id"]
	longURL, err := lengthenURL(ctx, short)
	if err != nil {
		respond(ctx, response{"", "unable to find URL!"}, http.StatusBadRequest, w)
		return
	}
	w.Header().Set("Location", longURL)
	w.WriteHeader(http.StatusMovedPermanently)
}

// Create a short URL and store the long one in GCS
func shortenURL(ctx context.Context, long string, code string) (string, error) {
	ctx, span := trace.StartSpan(ctx, "shortenURL")
	defer span.End()
	if code == "" {
		code = generateShortCode(ctx, long)
	}

	err := gcsWrite(ctx, code, long)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("https://%s/%s", os.Getenv("DOMAIN"), code), nil
}

// Recreate the full URL from the short code by reading from GCS
func lengthenURL(ctx context.Context, short string) (string, error) {
	ctx, span := trace.StartSpan(ctx, "lengthenURL")
	defer span.End()
	return gcsRead(ctx, short)
}

// Primitive to write an arbitrary string to a GCS object
func gcsWrite(ctx context.Context, short string, url string) error {
	ctx, span := trace.StartSpan(ctx, "gcsWrite")
	defer span.End()

	client, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}

	bucket := client.Bucket(os.Getenv("BUCKET"))
	object := bucket.Object(short)
	writer := object.NewWriter(ctx)

	_, err = fmt.Fprintf(writer, url)
	if err != nil {
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	err = client.Close()
	if err != nil {
		return err
	}

	return nil
}

// Primitive to read an arbitrary string from a GCS object
func gcsRead(ctx context.Context, short string) (string, error) {
	ctx, span := trace.StartSpan(ctx, "gcsRead")
	defer span.End()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return "", err
	}

	bucket := client.Bucket(os.Getenv("BUCKET"))
	object := bucket.Object(short)

	reader, err := object.NewReader(ctx)
	if err != nil {
		return "", err
	}

	buffer := new(bytes.Buffer)
	buffer.ReadFrom(reader)

	err = reader.Close()
	if err != nil {
		return "", err
	}

	err = client.Close()
	if err != nil {
		return "", err
	}

	return buffer.String(), nil
}

// Create a URL-friendly short code with a dense name
func generateShortCode(ctx context.Context, url string) string {
	ctx, span := trace.StartSpan(ctx, "generateShortCode")
	defer span.End()
	crc32 := crc32.ChecksumIEEE([]byte(url))
	num := make([]byte, 4)
	binary.LittleEndian.PutUint32(num, crc32)
	code := base58.Encode(num)
	return code
}

// Respond to all HTTP requests
func respond(ctx context.Context, resp response, code int, writer http.ResponseWriter) {
	ctx, span := trace.StartSpan(ctx, "respond")
	defer span.End()
	marshalled, err := json.Marshal(resp)
	if err != nil {
		log.Println(err)
	}
	writer.WriteHeader(code)
	writer.Write(marshalled)
}
