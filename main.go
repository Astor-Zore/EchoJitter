package main

import (
	"bytes"
	"embed"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
)

//go:embed index.html
var htmlFS embed.FS

// ────────────────────────────────────────────────────
// WAV I/O
// ────────────────────────────────────────────────────
type WavData struct {
	SampleRate int
	Samples    []float64
	Duration   float64
	FileName   string
}

func ReadWav(path string) (*WavData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseWav(f, path)
}

func ReadWavFromBytes(data []byte, name string) (*WavData, error) {
	r := bytes.NewReader(data)
	return parseWav(r, name)
}

func WriteWav(w io.Writer, samples []float64, sampleRate int, bitDepth int, numChannels int) error {
	numSamples := len(samples)
	bytesPerSample := bitDepth / 8
	blockAlign := numChannels * bytesPerSample
	dataSize := uint32(numSamples * bytesPerSample)
	fileSize := uint32(36 + dataSize)

	writeLE := func(v interface{}) { binary.Write(w, binary.LittleEndian, v) }

	io.WriteString(w, "RIFF")
	writeLE(fileSize)
	io.WriteString(w, "WAVE")
	io.WriteString(w, "fmt ")
	writeLE(uint32(16)) // chunk size
	writeLE(uint16(1))  // PCM
	writeLE(uint16(numChannels))
	writeLE(uint32(sampleRate))
	writeLE(uint32(sampleRate * blockAlign))
	writeLE(uint16(blockAlign))
	writeLE(uint16(bitDepth))
	io.WriteString(w, "data")
	writeLE(dataSize)

	switch bitDepth {
	case 16:
		for _, s := range samples {
			v := int16(math.Max(-32768, math.Min(32767, math.Round(s*32767))))
			writeLE(v)
		}
	case 32:
		for _, s := range samples {
			v := int32(math.Max(-2147483648, math.Min(2147483647, float64(s*2147483647))))
			writeLE(v)
		}
	}
	return nil
}

func WriteWavFile(path string, samples []float64, sampleRate int, bitDepth int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteWav(f, samples, sampleRate, bitDepth, 1)
}

func parseWav(r io.Reader, name string) (*WavData, error) {
	var riff [4]byte
	io.ReadFull(r, riff[:])
	if string(riff[:]) != "RIFF" {
		return nil, fmt.Errorf("not a RIFF file")
	}

	var fileSize uint32
	binary.Read(r, binary.LittleEndian, &fileSize)

	var wave [4]byte
	io.ReadFull(r, wave[:])
	if string(wave[:]) != "WAVE" {
		return nil, fmt.Errorf("not a WAVE file")
	}

	var sampleRate int
	var numChannels uint16
	var bitsPerSample uint16
	var dataSize uint32

	for {
		var chunkID [4]byte
		var chunkSize uint32
		_, err := io.ReadFull(r, chunkID[:])
		if err != nil {
			break
		}
		binary.Read(r, binary.LittleEndian, &chunkSize)

		switch string(chunkID[:]) {
		case "fmt ":
			startPos := int64(0) // placeholder; we don't have Seek
			var audioFormat uint16
			binary.Read(r, binary.LittleEndian, &audioFormat)
			binary.Read(r, binary.LittleEndian, &numChannels)
			var sr uint32
			binary.Read(r, binary.LittleEndian, &sr)
			sampleRate = int(sr)
			// skip byteRate(4) + blockAlign(2)
			skip := make([]byte, 6)
			io.ReadFull(r, skip)
			binary.Read(r, binary.LittleEndian, &bitsPerSample)
			_ = startPos
			remaining := int64(chunkSize) - 16
			if remaining > 0 {
				io.CopyN(io.Discard, r, remaining)
			}

		case "data":
			dataSize = chunkSize
			numSamples := int(dataSize) / int(numChannels) / int(bitsPerSample/8)
			samples := make([]float64, numSamples)
			raw := make([]byte, dataSize)
			io.ReadFull(r, raw)

			switch bitsPerSample {
			case 16:
				for i := 0; i < numSamples; i++ {
					off := i * int(numChannels) * 2
					val := int16(binary.LittleEndian.Uint16(raw[off : off+2]))
					samples[i] = float64(val) / 32768.0
				}
			case 32:
				for i := 0; i < numSamples; i++ {
					off := i * int(numChannels) * 4
					val := int32(binary.LittleEndian.Uint32(raw[off : off+4]))
					samples[i] = float64(val) / 2147483648.0
				}
			}

			if numChannels > 1 {
				bytesPerSampleFrame := int(numChannels) * int(bitsPerSample/8)
				mono := make([]float64, int(dataSize)/bytesPerSampleFrame)
				for i := range mono {
					mono[i] = samples[i*int(numChannels)]
				}
				samples = mono
			}

			return &WavData{
				SampleRate: sampleRate,
				Samples:    samples,
				Duration:   float64(len(samples)) / float64(sampleRate),
				FileName:   name,
			}, nil

		default:
			io.CopyN(io.Discard, r, int64(chunkSize))
		}
	}
	return nil, fmt.Errorf("no data chunk found")
}

// ────────────────────────────────────────────────────
// FFT (radix-2, in-place)
// ────────────────────────────────────────────────────
func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func fft(x []complex128) {
	n := len(x)
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		half := length >> 1
		angle := -2.0 * math.Pi / float64(length)
		wlen := complex(math.Cos(angle), math.Sin(angle))
		for i := 0; i < n; i += length {
			w := complex(1.0, 0.0)
			for j := 0; j < half; j++ {
				u := x[i+j]
				v := x[i+j+half] * w
				x[i+j] = u + v
				x[i+j+half] = u - v
				w *= wlen
			}
		}
	}
}

func ifft(x []complex128) {
	for i := range x {
		x[i] = complex(real(x[i]), -imag(x[i]))
	}
	fft(x)
	s := 1.0 / float64(len(x))
	for i := range x {
		x[i] = complex(real(x[i])*s, imag(x[i])*s)
	}
}

// ────────────────────────────────────────────────────
// Signal processing helpers
// ────────────────────────────────────────────────────
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a < b {
		return b
	}
	return a
}

func removeDC(x []float64) []float64 {
	sum := 0.0
	for _, v := range x {
		sum += v
	}
	mean := sum / float64(len(x))
	result := make([]float64, len(x))
	for i, v := range x {
		result[i] = v - mean
	}
	return result
}

func corr(a, b []float64) []float64 {
	outLen := len(a) + len(b) - 1
	n := nextPow2(outLen)

	A := make([]complex128, n)
	B := make([]complex128, n)
	for i, v := range a {
		A[i] = complex(v, 0)
	}
	for i, v := range b {
		B[i] = complex(v, 0)
	}

	fft(A)
	fft(B)
	for i := range A {
		A[i] *= cmplx.Conj(B[i])
	}
	ifft(A)

	result := make([]float64, outLen)
	for i := range result {
		result[i] = real(A[i])
	}
	return result
}

func normalizedXC(template, signal []float64, absOut bool) []float64 {
	if len(template) > len(signal) {
		return nil
	}
	c := corr(signal, template)
	valid := len(signal) - len(template) + 1

	tplE := 0.0
	for _, v := range template {
		tplE += v * v
	}

	cumSum := make([]float64, len(signal)+1)
	for i, v := range signal {
		cumSum[i+1] = cumSum[i] + v*v
	}

	result := make([]float64, valid)
	for i := 0; i < valid; i++ {
		segE := cumSum[i+len(template)] - cumSum[i]
		denom := math.Sqrt(tplE * segE)
		ncc := 0.0
		if denom > 1e-12 {
			ncc = c[i] / denom
		}
		if absOut {
			ncc = math.Abs(ncc)
		}
		result[i] = ncc
	}
	return result
}

func envelope(signal []float64, winMs int, fs int) []float64 {
	win := fs * winMs / 1000
	if win < 4 {
		win = 4
	}
	half := win / 2
	n := len(signal)
	absSig := make([]float64, n)
	for i, v := range signal {
		absSig[i] = math.Abs(v)
	}
	env := make([]float64, n)
	for i := 0; i < n; i++ {
		lo := max(0, i-half)
		hi := min(n, i+half)
		sum := 0.0
		for j := lo; j < hi; j++ {
			sum += absSig[j]
		}
		env[i] = sum / float64(hi-lo)
	}
	return env
}

func extractTemplate(signal []float64, fs int, chirpMs float64) ([]float64, int) {
	if chirpMs > 0 {
		win := int(chirpMs * float64(fs) / 1000)
		env := envelope(signal, 10, fs)
		maxVal, maxIdx := 0.0, 0
		for i, v := range env {
			if v > maxVal {
				maxVal = v
				maxIdx = i
			}
		}
		start := max(0, maxIdx-win/2)
		start = min(start, len(signal)-win)
		return signal[start : start+win], start
	}

	env := envelope(signal, 5, fs)
	maxEnv := 0.0
	for _, v := range env {
		if v > maxEnv {
			maxEnv = v
		}
	}
	if maxEnv < 1e-9 {
		n := min(int(0.1*float64(fs)), len(signal))
		return signal[:n], 0
	}

	thr := maxEnv * 0.5
	first := -1
	for i, v := range env {
		if v > thr {
			first = i
			break
		}
	}
	if first < 0 {
		n := min(int(0.1*float64(fs)), len(signal))
		return signal[:n], 0
	}

	last := first
	for last < len(signal) && env[last] > thr {
		last++
	}
	templateLen := last - first
	if templateLen < 100 {
		last = min(first+200, len(signal))
	}
	return signal[first:last], first
}

func findPeaks(x []float64, threshold float64, minDistance int) []int {
	peaks := []int{}
	for i := 1; i < len(x)-1; i++ {
		if x[i] > threshold && x[i] >= x[i-1] && x[i] >= x[i+1] {
			if x[i] == x[i-1] && x[i] == x[i+1] {
				continue
			}
			if len(peaks) == 0 || i-peaks[len(peaks)-1] >= minDistance {
				peaks = append(peaks, i)
			} else if x[i] > x[peaks[len(peaks)-1]] {
				peaks[len(peaks)-1] = i
			}
		}
	}
	return peaks
}

// ────────────────────────────────────────────────────
// Types
// ────────────────────────────────────────────────────
type AnalysisParams struct {
	Threshold float64 `json:"threshold"`
	MinGapSec float64 `json:"minGapSec"`
	Margin    int     `json:"margin"`
	UseBP     bool    `json:"useBP"`
	BPLow     int     `json:"bpLow"`
	BPHigh    int     `json:"bpHigh"`
	ChirpMs   float64 `json:"chirpMs"`
}

type AnalysisResult struct {
	DelaysSamples []int     `json:"delaysSamples"`
	DelaysMs      []float64 `json:"delaysMs"`
	CorrVals      []float64 `json:"corrVals"`
	PeakIndices   []int     `json:"peakIndices"`
	GlobalCC      []float64 `json:"globalCC"`
	RefPositions  []int     `json:"refPositions"`
	RecPositions  []int     `json:"recPositions"`
	Stats         Stats     `json:"stats"`
	SampleRate    int       `json:"sampleRate"`
	NumChirps     int       `json:"numChirps"`
	TemplateLen   int       `json:"templateLen"`
	DurationSec   float64   `json:"durationSec"`
}

type Stats struct {
	Count    int     `json:"count"`
	MaxSamp  int     `json:"maxSamp"`
	MinSamp  int     `json:"minSamp"`
	MeanSamp float64 `json:"meanSamp"`
	StdSamp  float64 `json:"stdSamp"`
	P2PSamp  int     `json:"p2pSamp"`
	MaxMs    float64 `json:"maxMs"`
	MinMs    float64 `json:"minMs"`
	MeanMs   float64 `json:"meanMs"`
	StdMs    float64 `json:"stdMs"`
	P2PMs    float64 `json:"p2pMs"`
}

type InitResponse struct {
	HasFiles       bool           `json:"hasFiles"`
	RefName        string         `json:"refName"`
	RecName        string         `json:"recName"`
	SampleRate     int            `json:"sampleRate"`
	DurationSec    float64        `json:"durationSec"`
	RefSamples     int            `json:"refSamples"`
	RecSamples     int            `json:"recSamples"`
	DefaultParams  AnalysisParams `json:"defaultParams"`
}

// ────────────────────────────────────────────────────
// Bandpass filter
// ────────────────────────────────────────────────────
func bandpassFilter(signal []float64, fs int, lowHz, highHz int) []float64 {
	n := nextPow2(len(signal))
	spec := make([]complex128, n)
	for i, v := range signal {
		spec[i] = complex(v, 0)
	}
	fft(spec)

	freqRes := float64(fs) / float64(n)
	lowBin := int(float64(lowHz) / freqRes)
	highBin := int(float64(highHz) / freqRes)
	if highBin > n/2 {
		highBin = n / 2
	}

	for i := 0; i < n; i++ {
		bin := i
		if bin > n/2 {
			bin = n - bin
		}
		if bin < lowBin || bin > highBin {
			spec[i] = 0
		}
	}

	ifft(spec)
	result := make([]float64, len(signal))
	for i := range result {
		result[i] = real(spec[i])
	}
	return result
}

// ────────────────────────────────────────────────────
// Signal generator
// ────────────────────────────────────────────────────
type GenConfig struct {
	SampleRate  int     `json:"sampleRate"`
	BitDepth    int     `json:"bitDepth"`
	Amplitude   float64 `json:"amplitude"`
	ChirpMs     float64 `json:"chirpMs"`
	GapMs       float64 `json:"gapMs"`
	RepeatCount int     `json:"repeatCount"`
	FreqStart   float64 `json:"freqStart"`
	FreqEnd     float64 `json:"freqEnd"`
}

func generateChirp(config GenConfig) []float64 {
	sr := float64(config.SampleRate)
	chirpLen := int(config.ChirpMs * sr / 1000)
	gapLen := int(config.GapMs * sr / 1000)
	if chirpLen < 4 {
		chirpLen = 4
	}

	f0 := config.FreqStart
	f1 := config.FreqEnd
	T := float64(chirpLen) / sr

	chirp := make([]float64, chirpLen)
	for i := 0; i < chirpLen; i++ {
		t := float64(i) / sr
		phase := 2 * math.Pi * (f0*t + (f1-f0)*t*t/(2*T))
		w := 0.5 * (1 - math.Cos(2*math.Pi*t/T))
		chirp[i] = config.Amplitude * math.Sin(phase) * w
	}

	totalLen := (chirpLen+gapLen)*config.RepeatCount - gapLen
	result := make([]float64, totalLen)
	for r := 0; r < config.RepeatCount; r++ {
		off := r * (chirpLen + gapLen)
		copy(result[off:off+chirpLen], chirp)
		for j := off + chirpLen; j < off+chirpLen+gapLen && j < totalLen; j++ {
			result[j] = 0
		}
	}

	return result
}

// ────────────────────────────────────────────────────
// Analysis engine
// ────────────────────────────────────────────────────
func analyze(refData, recData *WavData, params AnalysisParams) AnalysisResult {
	fs := refData.SampleRate
	n := min(len(refData.Samples), len(recData.Samples))

	ref := make([]float64, n)
	rec := make([]float64, n)
	copy(ref, refData.Samples[:n])
	copy(rec, recData.Samples[:n])

	ref = removeDC(ref)
	rec = removeDC(rec)

	if params.UseBP {
		ref = bandpassFilter(ref, fs, params.BPLow, params.BPHigh)
		rec = bandpassFilter(rec, fs, params.BPLow, params.BPHigh)
	}

	tpl, _ := extractTemplate(ref, fs, params.ChirpMs)
	ccRef := normalizedXC(tpl, ref, true)

	minDist := int(params.MinGapSec * float64(fs))
	if minDist < len(tpl) {
		minDist = len(tpl)
	}

	thr := params.Threshold
	if len(ccRef) > 0 && thr <= 0 {
		maxVal := 0.0
		for _, v := range ccRef {
			if v > maxVal {
				maxVal = v
			}
		}
		thr = maxVal * 0.3
	}

	maxCC := 0.0
	for _, v := range ccRef {
		if v > maxCC {
			maxCC = v
		}
	}
	absThr := thr
	if absThr < 1.0 && maxCC > 0 {
		absThr = thr * maxCC
	}

	refPeaks := findPeaks(ccRef, absThr, minDist)

	margin := params.Margin
	if margin <= 0 {
		margin = len(tpl) + 200
	}

	delays := make([]int, 0)
	delaysMs := make([]float64, 0)
	corrVals := make([]float64, 0)
	refPositions := make([]int, 0)
	recPositions := make([]int, 0)

	for _, rp := range refPeaks {
		lo := max(0, rp-margin)
		hi := min(len(rec)-len(tpl), rp+margin)
		if hi <= lo {
			continue
		}

		seg := rec[lo : hi+len(tpl)]
		ccl := normalizedXC(tpl, seg, true)

		if len(ccl) == 0 {
			continue
		}
		maxIdx := 0
		maxVal := ccl[0]
		for i := 1; i < len(ccl); i++ {
			if ccl[i] > maxVal {
				maxVal = ccl[i]
				maxIdx = i
			}
		}
		recPos := lo + maxIdx
		delay := recPos - rp

		delays = append(delays, delay)
		delaysMs = append(delaysMs, float64(delay)/float64(fs)*1000)
		corrVals = append(corrVals, maxVal)
		refPositions = append(refPositions, rp)
		recPositions = append(recPositions, recPos)
	}

	var globalCC []float64
	if len(ccRef) > 0 {
		dsFactor := max(1, len(ccRef)/4000)
		for i := 0; i < len(ccRef); i += dsFactor {
			globalCC = append(globalCC, ccRef[i])
		}
	}

	d := make([]float64, len(delays))
	for i, v := range delays {
		d[i] = float64(v)
	}

	stats := Stats{}
	if len(d) > 0 {
		sort.Float64s(d)
		stats.Count = len(d)
		stats.MinSamp = int(math.Round(d[0]))
		stats.MaxSamp = int(math.Round(d[len(d)-1]))
		stats.P2PSamp = stats.MaxSamp - stats.MinSamp

		mean := 0.0
		for _, v := range d {
			mean += v
		}
		mean /= float64(len(d))
		stats.MeanSamp = mean

		variance := 0.0
		for _, v := range d {
			diff := v - mean
			variance += diff * diff
		}
		if len(d) > 1 {
			variance /= float64(len(d) - 1)
		}
		stats.StdSamp = math.Sqrt(variance)

		msFac := 1000.0 / float64(fs)
		stats.MinMs = d[0] * msFac
		stats.MaxMs = d[len(d)-1] * msFac
		stats.P2PMs = float64(stats.P2PSamp) * msFac
		stats.MeanMs = mean * msFac
		stats.StdMs = math.Sqrt(variance) * msFac
	}

	return AnalysisResult{
		DelaysSamples: delays,
		DelaysMs:      delaysMs,
		CorrVals:      corrVals,
		PeakIndices:   refPeaks,
		GlobalCC:      globalCC,
		RefPositions:  refPositions,
		RecPositions:  recPositions,
		Stats:         stats,
		SampleRate:    fs,
		NumChirps:     len(refPeaks),
		TemplateLen:   len(tpl),
		DurationSec:   float64(n) / float64(fs),
	}
}

// ────────────────────────────────────────────────────
// Global state
// ────────────────────────────────────────────────────
var (
	gRef    *WavData
	gRec    *WavData
	gLastResult *AnalysisResult

	gDefaultParams = AnalysisParams{
		Threshold: 0.15,
		MinGapSec: 0.5,
		Margin:    2000,
		ChirpMs:   100,
		BPLow:     500,
		BPHigh:    9000,
	}

	gDefaultGen = GenConfig{
		SampleRate:  16000,
		BitDepth:    16,
		Amplitude:   0.8,
		ChirpMs:     200,
		GapMs:       2000,
		RepeatCount: 10,
		FreqStart:   500,
		FreqEnd:     8000,
	}

	cliRefPath = ""
	cliRecPath = ""

	mu sync.Mutex
)

// ────────────────────────────────────────────────────
// API handlers
// ────────────────────────────────────────────────────
func apiInit(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	resp := InitResponse{
		HasFiles:      gRef != nil && gRec != nil,
		DefaultParams: gDefaultParams,
	}

	if gRef != nil {
		resp.RefName = gRef.FileName
		resp.SampleRate = gRef.SampleRate
		resp.DurationSec = gRef.Duration
		resp.RefSamples = len(gRef.Samples)
	}
	if gRec != nil {
		resp.RecName = gRec.FileName
		resp.RecSamples = len(gRec.Samples)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func apiUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(200 << 20); err != nil {
		http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	var errs []string

	refFile, refHeader, err := r.FormFile("ref")
	if err != nil {
		errs = append(errs, "ref file required: "+err.Error())
	} else {
		data, err := io.ReadAll(refFile)
		refFile.Close()
		if err != nil {
			errs = append(errs, "failed reading ref: "+err.Error())
		} else {
			wd, err := ReadWavFromBytes(data, refHeader.Filename)
			if err != nil {
				errs = append(errs, "invalid ref WAV: "+err.Error())
			} else {
				mu.Lock()
				gRef = wd
				mu.Unlock()
			}
		}
	}

	recFile, recHeader, err := r.FormFile("rec")
	if err != nil {
		errs = append(errs, "rec file required: "+err.Error())
	} else {
		data, err := io.ReadAll(recFile)
		recFile.Close()
		if err != nil {
			errs = append(errs, "failed reading rec: "+err.Error())
		} else {
			wd, err := ReadWavFromBytes(data, recHeader.Filename)
			if err != nil {
				errs = append(errs, "invalid rec WAV: "+err.Error())
			} else {
				mu.Lock()
				gRec = wd
				mu.Unlock()
			}
		}
	}

	if len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "upload errors",
			"details": errs,
		})
		return
	}

	// Validate
	mu.Lock()
	defer mu.Unlock()
	if gRef != nil && gRec != nil && gRef.SampleRate != gRec.SampleRate {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("sample rate mismatch: %d vs %d", gRef.SampleRate, gRec.SampleRate),
		})
		gRef = nil
		gRec = nil
		return
	}

	// Return InitResponse
	resp := InitResponse{
		HasFiles:     true,
		DefaultParams: gDefaultParams,
	}
	if gRef != nil {
		resp.RefName = gRef.FileName
		resp.SampleRate = gRef.SampleRate
		resp.DurationSec = gRef.Duration
		resp.RefSamples = len(gRef.Samples)
	}
	if gRec != nil {
		resp.RecName = gRec.FileName
		resp.RecSamples = len(gRec.Samples)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func apiAnalyze(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	ref := gRef
	rec := gRec
	mu.Unlock()

	if ref == nil || rec == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "No files loaded. Upload ref and rec WAV files first."})
		return
	}

	q := r.URL.Query()
	params := gDefaultParams // copy defaults

	if v, err := strconv.ParseFloat(q.Get("threshold"), 64); err == nil {
		params.Threshold = v
	}
	if v, err := strconv.ParseFloat(q.Get("minGapSec"), 64); err == nil {
		params.MinGapSec = v
	}
	if v, err := strconv.Atoi(q.Get("margin")); err == nil {
		params.Margin = v
	}
	if q.Get("useBP") == "1" {
		params.UseBP = true
	}
	if v, err := strconv.Atoi(q.Get("bpLow")); err == nil {
		params.BPLow = v
	}
	if v, err := strconv.Atoi(q.Get("bpHigh")); err == nil {
		params.BPHigh = v
	}
	if v, err := strconv.ParseFloat(q.Get("chirpMs"), 64); err == nil {
		params.ChirpMs = v
	}

	result := analyze(ref, rec, params)

	mu.Lock()
	gLastResult = &result
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func apiExport(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	result := gLastResult
	mu.Unlock()

	if result == nil || len(result.DelaysSamples) == 0 {
		http.Error(w, "No analysis results to export", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=latency_result.csv")

	writer := csv.NewWriter(w)
	writer.Write([]string{"playback", "ref_pos", "rec_pos", "delay_samples", "delay_ms", "correlation"})
	for i := range result.DelaysSamples {
		writer.Write([]string{
			strconv.Itoa(i + 1),
			strconv.Itoa(result.RefPositions[i]),
			strconv.Itoa(result.RecPositions[i]),
			strconv.Itoa(result.DelaysSamples[i]),
			fmt.Sprintf("%.3f", result.DelaysMs[i]),
			fmt.Sprintf("%.4f", result.CorrVals[i]),
		})
	}
	// Summary rows
	writer.Write([]string{})
	writer.Write([]string{"statistic", "", "", "value_samples", "value_ms", ""})
	if result.Stats.Count > 0 {
		writer.Write([]string{"count", "", "", strconv.Itoa(result.Stats.Count), "", ""})
		writer.Write([]string{"max", "", "", strconv.Itoa(result.Stats.MaxSamp), fmt.Sprintf("%.3f", result.Stats.MaxMs), ""})
		writer.Write([]string{"min", "", "", strconv.Itoa(result.Stats.MinSamp), fmt.Sprintf("%.3f", result.Stats.MinMs), ""})
		writer.Write([]string{"mean", "", "", fmt.Sprintf("%.2f", result.Stats.MeanSamp), fmt.Sprintf("%.3f", result.Stats.MeanMs), ""})
		writer.Write([]string{"std_jitter", "", "", fmt.Sprintf("%.2f", result.Stats.StdSamp), fmt.Sprintf("%.3f", result.Stats.StdMs), ""})
		writer.Write([]string{"p2p_jitter", "", "", strconv.Itoa(result.Stats.P2PSamp), fmt.Sprintf("%.3f", result.Stats.P2PMs), ""})
	}
	writer.Flush()
}

func apiGenerateConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gDefaultGen)
}

func apiGenerate(w http.ResponseWriter, r *http.Request) {
	var config GenConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// apply defaults for zero values
	if config.SampleRate == 0 {
		config.SampleRate = gDefaultGen.SampleRate
	}
	if config.BitDepth == 0 {
		config.BitDepth = gDefaultGen.BitDepth
	}
	if config.Amplitude == 0 {
		config.Amplitude = gDefaultGen.Amplitude
	}
	if config.ChirpMs == 0 {
		config.ChirpMs = gDefaultGen.ChirpMs
	}
	if config.GapMs == 0 {
		config.GapMs = gDefaultGen.GapMs
	}
	if config.RepeatCount == 0 {
		config.RepeatCount = gDefaultGen.RepeatCount
	}
	if config.FreqStart == 0 {
		config.FreqStart = gDefaultGen.FreqStart
	}
	if config.FreqEnd == 0 {
		config.FreqEnd = gDefaultGen.FreqEnd
	}

	samples := generateChirp(config)
	duration := float64(len(samples)) / float64(config.SampleRate)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=chirp_%dHz_%dms_%d.wav",
			config.SampleRate, int(config.ChirpMs), config.RepeatCount))
	WriteWav(w, samples, config.SampleRate, config.BitDepth, 1)

	// Log to server
	fmt.Printf("Generated: %d samples, %.1fs, %dHz/%dbit, %d repeats\n",
		len(samples), duration, config.SampleRate, config.BitDepth, config.RepeatCount)
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	data, _ := htmlFS.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ────────────────────────────────────────────────────
// Main
// ────────────────────────────────────────────────────
func parseFlags() (refPath, recPath string, port int, noWeb bool, genOutput string) {
	refPath = ""
	recPath = ""
	port = 8080
	noWeb = false
	genOutput = ""

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--ref":
			i++
			if i < len(os.Args) {
				refPath = os.Args[i]
			}
		case "--rec":
			i++
			if i < len(os.Args) {
				recPath = os.Args[i]
			}
		case "--port":
			i++
			if i < len(os.Args) {
				port, _ = strconv.Atoi(os.Args[i])
			}
		case "--no-web":
			noWeb = true
		case "--generate":
			i++
			if i < len(os.Args) {
				genOutput = os.Args[i]
			}
		case "--gen-sr":
			i++
			if i < len(os.Args) {
				v, _ := strconv.Atoi(os.Args[i])
				gDefaultGen.SampleRate = v
			}
		case "--gen-bits":
			i++
			if i < len(os.Args) {
				v, _ := strconv.Atoi(os.Args[i])
				gDefaultGen.BitDepth = v
			}
		case "--gen-amp":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultGen.Amplitude = v
			}
		case "--gen-chirp-ms":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultGen.ChirpMs = v
			}
		case "--gen-gap-ms":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultGen.GapMs = v
			}
		case "--gen-repeat":
			i++
			if i < len(os.Args) {
				v, _ := strconv.Atoi(os.Args[i])
				gDefaultGen.RepeatCount = v
			}
		case "--gen-f0":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultGen.FreqStart = v
			}
		case "--gen-f1":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultGen.FreqEnd = v
			}
		case "--threshold":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultParams.Threshold = v
			}
		case "--chirp-ms":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultParams.ChirpMs = v
			}
		case "--margin":
			i++
			if i < len(os.Args) {
				v, _ := strconv.Atoi(os.Args[i])
				gDefaultParams.Margin = v
			}
		case "--min-gap":
			i++
			if i < len(os.Args) {
				v, _ := strconv.ParseFloat(os.Args[i], 64)
				gDefaultParams.MinGapSec = v
			}
		case "--help":
			fmt.Print(`Latency / Jitter Measurement Tool (Go)

Usage:
  latency_jitter                           Start Web UI (no files preloaded)
  latency_jitter --generate output.wav     Generate test chirp WAV and exit
  latency_jitter --ref ref.wav --rec rec.wav
                                           Start Web UI with files preloaded
  latency_jitter --ref ref.wav --rec rec.wav --no-web
                                           CLI mode: run analysis and print result

Analysis options:
  --ref <path>       Reference WAV (playback signal)
  --rec <path>       Recording WAV (DMIC capture)
  --port N           Web server port (default: 8080)
  --no-web           Run CLI-only, no web server
  --threshold 0.15   Cross-correlation peak threshold [0-1]
  --chirp-ms 100     Chirp template duration in ms (0=auto)
  --margin 2000      Local search window in samples
  --min-gap 0.5      Minimum gap between chirps in seconds

Generation options (use with --generate output.wav):
  --generate <path>  Generate test signal and save to file
  --gen-sr 16000     Sample rate (Hz)
  --gen-bits 16      Bit depth (16 or 32)
  --gen-amp 0.8      Amplitude (0.0 - 1.0)
  --gen-chirp-ms 200 Chirp duration (ms)
  --gen-gap-ms 2000  Silence gap between chirps (ms)
  --gen-repeat 10    Number of chirp repetitions
  --gen-f0 500       Chirp start frequency (Hz)
  --gen-f1 8000      Chirp end frequency (Hz)

Example:
  latency_jitter --generate test.wav --gen-sr 16000 --gen-chirp-ms 200 --gen-repeat 10
  latency_jitter --ref test.wav --rec recording.wav

Web UI: open http://localhost:8080, select files, adjust params, export CSV.
`)
			os.Exit(0)
		}
	}
	return
}

func printResult(result AnalysisResult, params AnalysisParams) {
	fmt.Printf("=== Analysis (threshold=%.2f, minGap=%.1fs, margin=%d) ===\n",
		params.Threshold, params.MinGapSec, params.Margin)
	fmt.Printf("Template length: %d samples (%.1f ms)\n",
		result.TemplateLen, float64(result.TemplateLen)/float64(result.SampleRate)*1000)
	fmt.Printf("Chirps detected in ref: %d\n", result.NumChirps)
	fmt.Printf("Valid delays found:     %d\n", result.Stats.Count)

	if result.Stats.Count > 0 {
		fmt.Println()
		fmt.Printf("%-5s %-12s %-12s %-14s %-10s %-6s\n",
			"No.", "ref_pos", "rec_pos", "delay(samp)", "delay(ms)", "corr")
		for i := range result.DelaysSamples {
			fmt.Printf("%-5d %-12d %-12d %-14d %-10.3f %-6.3f\n",
				i+1, result.RefPositions[i], result.RecPositions[i],
				result.DelaysSamples[i], result.DelaysMs[i], result.CorrVals[i])
		}
		fmt.Println()
		fmt.Println("=== Statistics ===")
		fmt.Printf("Count (N)    : %d\n", result.Stats.Count)
		fmt.Printf("Max delay    : %d samples (%.3f ms)\n", result.Stats.MaxSamp, result.Stats.MaxMs)
		fmt.Printf("Min delay    : %d samples (%.3f ms)\n", result.Stats.MinSamp, result.Stats.MinMs)
		fmt.Printf("Mean delay   : %.1f samples (%.3f ms)\n", result.Stats.MeanSamp, result.Stats.MeanMs)
		fmt.Printf("Std (jitter) : %.2f samples (%.3f ms)\n", result.Stats.StdSamp, result.Stats.StdMs)
		fmt.Printf("P2P (jitter) : %d samples (%.3f ms)\n", result.Stats.P2PSamp, result.Stats.P2PMs)
	}
}

func main() {
	refPath, recPath, port, noWeb, genOutput := parseFlags()

	// Handle --generate (CLI-only generation)
	if genOutput != "" {
		fmt.Println("=== Signal Generator ===")
		fmt.Printf("Sample rate : %d Hz\n", gDefaultGen.SampleRate)
		fmt.Printf("Bit depth   : %d\n", gDefaultGen.BitDepth)
		fmt.Printf("Chirp       : %d ms, %.0f - %.0f Hz\n",
			int(gDefaultGen.ChirpMs), gDefaultGen.FreqStart, gDefaultGen.FreqEnd)
		fmt.Printf("Gap         : %d ms\n", int(gDefaultGen.GapMs))
		fmt.Printf("Repeats     : %d\n", gDefaultGen.RepeatCount)

		samples := generateChirp(gDefaultGen)
		duration := float64(len(samples)) / float64(gDefaultGen.SampleRate)

		if err := WriteWavFile(genOutput, samples, gDefaultGen.SampleRate, gDefaultGen.BitDepth); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Output      : %s\n", genOutput)
		fmt.Printf("Total       : %d samples (%.2f s)\n", len(samples), duration)
		return
	}

	cliRefPath = refPath
	cliRecPath = recPath

	// Load files if provided via CLI
	if refPath != "" && recPath != "" {
		fmt.Println("=== Latency / Jitter Measurement Tool ===")
		fmt.Printf("Reference: %s\n", refPath)
		fmt.Printf("Recording: %s\n", recPath)
		var err error
		gRef, err = ReadWav(refPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading ref: %v\n", err)
			if noWeb {
				os.Exit(1)
			}
			gRef = nil
		}
		gRec, err = ReadWav(recPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading rec: %v\n", err)
			if noWeb {
				os.Exit(1)
			}
			gRec = nil
		}
		if gRef != nil && gRec != nil {
			if gRef.SampleRate != gRec.SampleRate {
				fmt.Fprintf(os.Stderr, "Sample rate mismatch: %d vs %d\n", gRef.SampleRate, gRec.SampleRate)
				if noWeb {
					os.Exit(1)
				}
				gRef = nil
				gRec = nil
			} else {
				fmt.Printf("Sample rate: %d Hz, Duration: %.3f s (%d samples)\n\n",
					gRef.SampleRate, gRef.Duration, len(gRef.Samples))

				// Run initial analysis
				result := analyze(gRef, gRec, gDefaultParams)
				gLastResult = &result
				printResult(result, gDefaultParams)
			}
		}
	}

	if noWeb {
		return
	}

	fmt.Printf("\nOpen http://localhost:%d in your browser.\n", port)
	fmt.Println("Press Ctrl+C to quit.")

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/api/init", apiInit)
	http.HandleFunc("/api/upload", apiUpload)
	http.HandleFunc("/api/analyze", apiAnalyze)
	http.HandleFunc("/api/export", apiExport)
	http.HandleFunc("/api/generate-config", apiGenerateConfig)
	http.HandleFunc("/api/generate", apiGenerate)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}