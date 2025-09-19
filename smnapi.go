// Creador de API del SMN de la CONAGUA

// Ejecutar en el directorio de este archivo.
// go mod init smnapi
// go mod tidy
// go run smnapi.go

//

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// CONFIG: Constantes de configuración del servicio
// CONFIG
const (
	FUENTE        = "https://smn.conagua.gob.mx/tools/GUI/webservices/index.php?method=1"				// URL fuente de datos meteorológicos
	UPDATE_PERIOD = 3 * time.Hour		// Periodo de actualización de datos
	HTTP_PORT     = "5642"			// Puerto HTTP del servidor
	MAX_BODY      = 200 << 20		// Límite máximo de respuesta 200MB safety cap
	USER_AGENT    = "smn-cache-go/1.1"	// User-Agent para las peticiones
	RETRY_COUNT   = 3			// Intentos para descargas
	RETRY_BACKOFF = 3 * time.Second		// Tiempo de espera entre reintentos
)

// rawEntry representa una entrada cruda del JSON fuente (todos los campos son strings)
// Raw entry matches the JSON items in source (all strings)
type rawEntry map[string]string

// ForecastDay contiene los campos normalizados de pronóstico para un día
// ForecastDay holds normalized fields we care about
type ForecastDay struct {
	NDia     int      `json:"ndia"`			// Número de día (0-3)
	DLoc     string   `json:"dloc"`			// Fecha localizada
	TMin     *float64 `json:"tmin,omitempty"`	// Temperatura mínima
	TMax     *float64 `json:"tmax,omitempty"`	// Temperatura máxima
	ProbPrec *int     `json:"probprec,omitempty"`	// Probabilidad de precipitación
	Prec     *float64 `json:"prec,omitempty"`	// Precipitación
	Desciel  string   `json:"desciel,omitempty"`	// Descripción del cielo
	VelVien  *float64 `json:"velvien,omitempty"`	// Velocidad del viento
	DirVienG *int     `json:"dirvieng,omitempty"`	// Dirección del viento en grados
	DirVienC string   `json:"dirvienc,omitempty"`	// Dirección cardinal del viento
	CC       *int     `json:"cc,omitempty"`		// Cobertura de nubes
	Raf      *float64 `json:"raf,omitempty"`	// (ráfagas de viento)
}

// MunForecast representa el pronóstico completo de un municipio
// Municipality forecast object returned by API
type MunForecast struct {
	IDMun      int           `json:"idmun"`		// ID del municipio
	NMun       string        `json:"nmun"`		// Nombre del municipio
	IDEs       int           `json:"ides"`		// ID del estado
	NES        string        `json:"nes,omitempty"`	// Nombre del estado
	LastUpdate time.Time     `json:"last_update"`	// Última actualización
	Days       []ForecastDay `json:"days"`		// Pronóstico por días (0-3)
	Lat        *float64      `json:"lat,omitempty"`	// Latitud
	Lon        *float64      `json:"lon,omitempty"`	// Longitud
	Dh         *int          `json:"dh,omitempty"`	// Diferencia horaria UTC
}

// MunSearchResult representa un resultado de búsqueda de municipio
// For search results (compact)
type MunSearchResult struct {
	IDMun int    `json:"idmun"`	// ID del municipio
	IDEs  int    `json:"ides"`	// ID del estado
	NES   string `json:"nes"`	// Nombre del estado
	NMun  string `json:"nmun"`	// Nombre del municipio
	Lat   *float64 `json:"lat,omitempty"`	// Latitud
	Lon   *float64 `json:"lon,omitempty"`	// Longitud
}

// Cache estructura que almacena los datos meteorológicos en memoria
// cache structure
type Cache struct {
	sync.RWMutex			// Mutex para acceso concurrente
	Data         map[string]MunForecast // Datos principales ("ides-idmun" -> pronóstico)
	LastUpdate   time.Time		// Última actualización
	Status       string		// Estado del cache
	nameIndex    map[string][]string // Índice de búsqueda (clave normalizada -> claves compuestas) ("ides-idmun")
	indexBuiltAt time.Time		// Cuándo se construyó el índice

}

// Variable global que almacena la caché
var cache = Cache{
	Data:       make(map[string]MunForecast),
	LastUpdate: time.Time{},
	Status:     "init",
	nameIndex:  make(map[string][]string),
}

func main() {
	// Contexto para apagado graceful
	// Create a cancelable context tied to SIGINT/SIGTERM for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Manejo de señales para apagado graceful
	// Signal handler to cancel context on termination signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutdown signal received, stopping updater")
		cancel()
	}()

	// Iniciar actualizador en background
	// Start background updater
	go updater(ctx)

	// Configurar handlers HTTP
	// HTTP handlers
	http.HandleFunc("/forecast/", corsMiddleware(handleForecast))
	http.HandleFunc("/status", corsMiddleware(handleStatus))
	http.HandleFunc("/search", corsMiddleware(handleSearch))

	// Iniciar servidor HTTP
	addr := ":" + HTTP_PORT
	log.Printf("starting server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// updater actualiza periódicamente la caché con datos de la fuente
// updater periodically fetches and updates cache
func updater(ctx context.Context) {
	// Carga inicial inmediata (con reintentos)
	// initial immediate load (with retries)
	if err := fetchAndLoad(); err != nil {
		log.Printf("initial load failed: %v", err)
		cache.Lock()
		cache.Status = "degraded: initial load failed"
		cache.Unlock()
	}
	// Programar actualizaciones periódicas
	ticker := time.NewTicker(UPDATE_PERIOD)
	defer ticker.Stop()
	for {
		select {
			case <-ctx.Done():
				log.Printf("updater exiting")
				return
			case <-ticker.C:
				if err := fetchAndLoad(); err != nil {
					log.Printf("fetchAndLoad error: %v", err)
					cache.Lock()
					cache.Status = fmt.Sprintf("degraded: last update failed at %s", time.Now().UTC().Format(time.RFC3339))
					cache.Unlock()
				}
		}
	}
}

// fetchAndLoad descarga, parsea y actualiza la caché atómicamente
// fetchAndLoad downloads JSON, parses and replaces cache atomically
func fetchAndLoad() error {
	var body []byte
	var err error
	for i := 0; i < RETRY_COUNT; i++ {
		body, err = downloadSource()
		if err == nil {
			break
		}
		log.Printf("download attempt %d failed: %v", i+1, err)
		time.Sleep(RETRY_BACKOFF * time.Duration(i+1))
	}
	if err != nil {
		return fmt.Errorf("download failed after retries: %w", err)
	}

	// Decode into []rawEntry directly from []byte to avoid needless conversions
	entries := make([]rawEntry, 0)
	if err := json.Unmarshal(body, &entries); err != nil {
		return fmt.Errorf("json unmarshal failed: %w", err)
	}

	newMap := make(map[string]MunForecast, len(entries)/4+10)

	// group by composite key (ides + idmun) and collect
	for _, re := range entries {
		idmun, err := atoiPtr(strings.TrimSpace(re["idmun"]))
		if err != nil || idmun == nil {
			continue
		}
		ides, err := atoiPtr(strings.TrimSpace(re["ides"]))
		if err != nil || ides == nil {
			continue
		}

		// Create composite key
		compositeKey := fmt.Sprintf("%d-%d", *ides, *idmun)

		ndia, _ := atoiPtr(re["ndia"])
		tmin, _ := atofPtr(re["tmin"])
		tmax, _ := atofPtr(re["tmax"])
		probprec, _ := atoiPtr(re["probprec"])
		prec, _ := atofPtr(re["prec"])
		vel, _ := atofPtr(re["velvien"])
		dirg, _ := atoiPtr(re["dirvieng"])
		dirvienc := strings.TrimSpace(re["dirvienc"])
		cc, _ := atoiPtr(re["cc"])
		raf, _ := atofPtr(re["raf"])
		lat, _ := atofPtr(re["lat"])
		lon, _ := atofPtr(re["lon"])
		dh, _ := atoiPtr(re["dh"])

		day := ForecastDay{
			NDia:    0,
			DLoc:    normalizeDloc(re["dloc"]),
			Desciel: re["desciel"],
			DirVienC: dirvienc,
			Raf:      raf,
		}
		if ndia != nil {
			day.NDia = *ndia
		}
		if tmin != nil {
			day.TMin = tmin
		}
		if tmax != nil {
			day.TMax = tmax
		}
		if probprec != nil {
			day.ProbPrec = probprec
		}
		if prec != nil {
			day.Prec = prec
		}
		if vel != nil {
			day.VelVien = vel
		}
		if dirg != nil {
			day.DirVienG = dirg
		}
		if cc != nil {
			day.CC = cc
		}

		mf, ok := newMap[compositeKey]
		if !ok {
			// Trim name fields to avoid index keys with surrounding spaces
			nmun := strings.TrimSpace(re["nmun"])
			nes := strings.TrimSpace(re["nes"])
			mf = MunForecast{
				IDMun:      *idmun,
				NMun:       nmun,
				IDEs:       *ides,
				NES:        nes,
				LastUpdate: time.Now().UTC(),
				Days:       make([]ForecastDay, 0, 4),
				Lat:        lat,  // Asignar latitud
				Lon:        lon,  // Asignar longitud
				Dh:         dh,   // Asignar diferencia horaria
			}
		}
		mf.Days = append(mf.Days, day)
		newMap[compositeKey] = mf
	}

	// Normalize days for each municipality (ensure indices 0..3 present)
	for key, mf := range newMap {
		// Create a map to group days by NDia
		daysByNDia := make(map[int]ForecastDay)
		for _, day := range mf.Days {
			if day.NDia >= 0 && day.NDia <= 3 {
				daysByNDia[day.NDia] = day
			}
		}

		// Create ordered slice with all 4 days
		orderedDays := make([]ForecastDay, 4)
		for i := 0; i < 4; i++ {
			if day, exists := daysByNDia[i]; exists {
				orderedDays[i] = day
			} else {
				orderedDays[i] = ForecastDay{NDia: i}
			}
		}
		mf.Days = orderedDays
		newMap[key] = mf
	}

	// Build name index
	nameIdx := buildNameIndex(newMap)

	// atomically replace cache
	cache.Lock()
	cache.Data = newMap
	cache.nameIndex = nameIdx
	cache.LastUpdate = time.Now().UTC()
	cache.indexBuiltAt = time.Now().UTC()
	cache.Status = "ok"
	cache.Unlock()
	log.Printf("cache updated: %d municipios (index %d keys)", len(newMap), len(nameIdx))
	return nil
}

// buildNameIndex crea un índice de búsqueda por nombres normalizados
// buildNameIndex creates map keys from normalized names to composite keys slices
func buildNameIndex(data map[string]MunForecast) map[string][]string {
	idx := make(map[string][]string)
	for compositeKey, mf := range data {
		// Normalizar nombres
		nmunNorm := normalizeText(mf.NMun)
		nesNorm := normalizeText(mf.NES)

		// Indexar por municipio solo
		idx[nmunNorm] = append(idx[nmunNorm], compositeKey)

		// Indexar por estado solo
		idx[nesNorm] = append(idx[nesNorm], compositeKey)

		// Indexar combinación estado|municipio
		compound := nesNorm + "|" + nmunNorm
		idx[compound] = append(idx[compound], compositeKey)

		// Indexar combinación municipio|estado
		reverse := nmunNorm + "|" + nesNorm
		idx[reverse] = append(idx[reverse], compositeKey)

		// Indexar palabras individuales para búsqueda parcial
		words := strings.Fields(nmunNorm)
		for _, word := range words {
			if len(word) > 2 { // Solo indexar palabras de 3+ caracteres
				idx[word] = append(idx[word], compositeKey)
			}
		}
	}
	return idx
}

// downloadSource descarga los datos de la fuente con manejo de gzip y límites
// downloadSource fetches the remote JSON with limit and gzip handling
func downloadSource() ([]byte, error) {
	req, _ := http.NewRequest("GET", FUENTE, nil)
	req.Header.Set("User-Agent", USER_AGENT)
	client := http.Client{
		Timeout: 60 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	// Leer todo en memoria (hasta MAX_BODY)
	limited := io.LimitReader(resp.Body, MAX_BODY)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	// Si alcanzamos o excedemos el límite, considerarlo error por seguridad
	if len(raw) >= int(MAX_BODY) {
		return nil, errors.New("response exceeded max allowed size")
	}

	// Detect gzip por header o magic bytes 0x1f 0x8b
	isGzip := false
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		isGzip = true
	} else if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		isGzip = true
	}

	if isGzip {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		unzipped, err := io.ReadAll(io.LimitReader(gr, MAX_BODY))
		if err != nil {
			return nil, err
		}
		// guard again after unzip
		if len(unzipped) >= int(MAX_BODY) {
			return nil, errors.New("unzipped response exceeded max allowed size")
		}
		return unzipped, nil
	}

	return raw, nil
}

// HTTP Handlers

// handleForecast maneja peticiones de pronóstico por ID de municipio y estado
// handleForecast returns forecast for composite key: GET /forecast/{ides}/{idmun}
func handleForecast(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/forecast/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "ides and idmun required", http.StatusBadRequest)
		return
	}
	idesStr := parts[0]
	idmunStr := parts[1]
	ides, err := strconv.Atoi(idesStr)
	if err != nil {
		http.Error(w, "invalid ides", http.StatusBadRequest)
		return
	}
	idmun, err := strconv.Atoi(idmunStr)
	if err != nil {
		http.Error(w, "invalid idmun", http.StatusBadRequest)
		return
	}
	compositeKey := fmt.Sprintf("%d-%d", ides, idmun)

	cache.RLock()
	mf, ok := cache.Data[compositeKey]
	last := cache.LastUpdate
	status := cache.Status
	cache.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-Cache-LastUpdate", last.Format(time.RFC3339))
	w.Header().Set("X-Cache-Status", status)

	enc := json.NewEncoder(w)
	if err := enc.Encode(mf); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
}

// handleSearch maneja búsquedas de municipios por nombre
// handleSearch supports:
// GET /search?mun=municipioName
// GET /search?mun=municipioName&estado=estadoName
// returns array of MunSearchResult
func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mun := strings.TrimSpace(q.Get("mun"))
	estado := strings.TrimSpace(q.Get("estado"))

	if mun == "" {
		http.Error(w, "parameter 'mun' required", http.StatusBadRequest)
		return
	}

	munNorm := normalizeText(mun)
	estadoNorm := normalizeText(estado)

	cache.RLock()
	defer cache.RUnlock()

	results := []MunSearchResult{}
	seen := make(map[string]bool)

	// Búsqueda por combinación estado|municipio
	if estadoNorm != "" {
		compoundKey := estadoNorm + "|" + munNorm
		if compositeKeys, ok := cache.nameIndex[compoundKey]; ok {
			addResults(compositeKeys, &results, seen, cache.Data)
		}
	}

	// Búsqueda por municipio solamente
	if compositeKeys, ok := cache.nameIndex[munNorm]; ok {
		addResults(compositeKeys, &results, seen, cache.Data)
	}

	// Búsqueda por palabras individuales (búsqueda parcial)
	searchWords := strings.Fields(munNorm)
	for _, word := range searchWords {
		if len(word) > 2 {
			if compositeKeys, ok := cache.nameIndex[word]; ok {
				addResults(compositeKeys, &results, seen, cache.Data)
			}
		}
	}

	// Búsqueda fuzzy como último recurso
	if len(results) == 0 {
		for compositeKey, mf := range cache.Data {
			if seen[compositeKey] {
				continue
			}
			nmunNorm := normalizeText(mf.NMun)
			nesNorm := normalizeText(mf.NES)

			if strings.Contains(nmunNorm, munNorm) ||
				(estadoNorm != "" && strings.Contains(nesNorm, estadoNorm)) {
					results = append(results, MunSearchResult{
						IDMun: mf.IDMun,
						IDEs:  mf.IDEs,
						NES:   mf.NES,
						NMun:  mf.NMun,
					})
					seen[compositeKey] = true
				}
		}
	}

	// Limitar resultados
	if len(results) > 50 {
		results = results[:50]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// addResults agrega resultados de búsqueda evitando duplicados
// addResults agrega resultados evitando duplicados
func addResults(compositeKeys []string, results *[]MunSearchResult, seen map[string]bool, data map[string]MunForecast) {
	for _, compositeKey := range compositeKeys {
		if !seen[compositeKey] {
			if mf, ok := data[compositeKey]; ok {
				*results = append(*results, MunSearchResult{
					IDMun: mf.IDMun,
					IDEs:  mf.IDEs,
					NES:   mf.NES,
					NMun:  mf.NMun,
					Lat:   mf.Lat,
					Lon:   mf.Lon,
				})
				seen[compositeKey] = true
			}
		}
	}
}

// handleStatus proporciona información del estado del servicio
// handleStatus returns basic status
func handleStatus(w http.ResponseWriter, r *http.Request) {
	cache.RLock()
	resp := map[string]interface{}{
		"status":      cache.Status,
		"last_update": cache.LastUpdate.Format(time.RFC3339),
		"count":       len(cache.Data),
		"now":         time.Now().UTC().Format(time.RFC3339),
		"index_keys":  len(cache.nameIndex),
		"index_built": cache.indexBuiltAt.Format(time.RFC3339),
	}
	cache.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(resp); err != nil {
		http.Error(w, "encoding error", http.StatusInternalServerError)
		return
	}
}

// Helpers
// Helpers: funciones auxiliares para procesamiento de datos

// atoiPtr convierte string a int pointer, manejando valores vacíos y floats
// atoiPtr returns *int or nil if empty/invalid
func atoiPtr(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// allow floats like "12.0"
	if strings.Contains(s, ".") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, err
		}
		i := int(f)
		return &i, nil
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// atofPtr convierte string a float64 pointer, manejando valores vacíos
// atofPtr returns *float64 or nil if empty/invalid
func atofPtr(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// normalizeDloc normaliza formato de fecha de la fuente a formato RFC3339
// normalizeDloc converts source "YYYYmmddThh" or "YYYYmmddThhmm" to RFC3339-like (no timezone)
func normalizeDloc(s string) string {
	if s == "" {
		return ""
	}
	s = strings.TrimSpace(s)
	if strings.Contains(s, "T") {
		parts := strings.SplitN(s, "T", 2)
		date := parts[0]
		timepart := parts[1]
		if len(date) == 8 {
			yr := date[0:4]
			mo := date[4:6]
			da := date[6:8]
			hh := "00"
			mm := "00"
			if len(timepart) >= 2 {
				hh = timepart[0:2]
			}
			if len(timepart) >= 4 {
				mm = timepart[2:4]
			}
			return fmt.Sprintf("%s-%s-%sT%s:%s:00", yr, mo, da, hh, mm)
		}
	}
	return s
}

// normalizeText normaliza texto para búsquedas: minúsculas, sin acentos, etc.
// normalizeText: lowercase, remove diacritics, collapse spaces, remove punctuation
func normalizeText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	// remove diacritics using unicode normalization
	t := norm.NFD.String(s)
	var b strings.Builder
	prevSpace := false
	for _, r := range t {
		// skip combining marks
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		// replace punctuation with space
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// corsMiddleware añade headers CORS para permitir peticiones desde cualquier origen
// Simple CORS middleware allowing all origins; adjust for production
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
