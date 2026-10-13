package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const (
	// "minimal" misread "50 lucas" as 50 and dropped amounts in ~1 of 8 parses; "low" got 40/40.
	reasoningEffort = "low"
	transcribeModel = "gpt-4o-mini-transcribe"
	parseModel      = "gpt-5-nano"
	// Context, not vocabulary: tells the transcriber what kind of speech to expect.
	transcribeContext = "Audio en español rioplatense registrando gastos: montos, nombres de comercios " +
		"y restaurantes, nombres de personas, medios de pago."
)

type OpenAI struct {
	key  string
	http *http.Client
}

func NewOpenAI(key string) *OpenAI {
	return &OpenAI{key: key, http: &http.Client{Timeout: 90 * time.Second}}
}

func (o *OpenAI) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+o.key)
	res, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai %s: %d %s", req.URL.Path, res.StatusCode, body)
	}
	return body, nil
}

func (o *OpenAI) Transcribe(ctx context.Context, audio []byte, filename string, terms []string) (string, error) {
	prompt := transcribeContext
	if len(terms) > 0 {
		prompt += " Nombres frecuentes: " + strings.Join(terms, ", ") + "."
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"model": transcribeModel, "language": "es", "prompt": prompt} {
		if err := w.WriteField(k, v); err != nil {
			return "", err
		}
	}
	// OpenAI picks the decoder from the filename extension.
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	body, err := o.do(req)
	if err != nil {
		return "", err
	}
	var out struct{ Text string }
	return out.Text, json.Unmarshal(body, &out)
}

type ParsedEntry struct {
	Amount        *float64 `json:"amount"`
	Currency      string   `json:"currency"`
	Category      *string  `json:"category"`
	Merchant      *string  `json:"merchant"`
	PaymentMethod *string  `json:"payment_method"`
	Note          string   `json:"note"`
	Date          string   `json:"date"`
}

func nullableEnum(values []string) map[string]any {
	enum := make([]any, 0, len(values)+1)
	for _, v := range values {
		enum = append(enum, v)
	}
	return map[string]any{"type": []string{"string", "null"}, "enum": append(enum, nil)}
}

func entriesSchema(cfg *Config) map[string]any {
	nullableString := map[string]any{"type": []string{"string", "null"}}
	entry := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"amount":         map[string]any{"type": []string{"number", "null"}},
			"currency":       map[string]any{"type": "string", "enum": []string{"ARS", "USD"}},
			"category":       nullableEnum(cfg.Categories),
			"merchant":       nullableString,
			"payment_method": nullableEnum(cfg.PaymentMethods),
			"note":           map[string]any{"type": "string"},
			"date":           map[string]any{"type": "string"},
		},
		"required":             []string{"amount", "currency", "category", "merchant", "payment_method", "note", "date"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"entries": map[string]any{"type": "array", "items": entry}},
		"required":             []string{"entries"},
		"additionalProperties": false,
	}
}

func (o *OpenAI) Parse(ctx context.Context, cfg *Config, transcript string, recordedAt time.Time) ([]ParsedEntry, error) {
	payload := map[string]any{
		"model":        parseModel,
		"instructions": cfg.Instructions(),
		"input":        fmt.Sprintf("Hoy es %s %s.\n\nTranscripción: %s", weekdays[recordedAt.Weekday()], recordedAt.Format("2006-01-02"), transcript),
		"reasoning":    map[string]any{"effort": reasoningEffort},
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "entries", "strict": true, "schema": entriesSchema(cfg),
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/responses", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	body, err := o.do(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Output []struct {
			Type    string
			Content []struct{ Type, Text string }
		}
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	for _, item := range res.Output {
		for _, c := range item.Content {
			if item.Type == "message" && c.Type == "output_text" {
				var parsed struct{ Entries []ParsedEntry }
				if err := json.Unmarshal([]byte(c.Text), &parsed); err != nil {
					return nil, fmt.Errorf("parse output: %w", err)
				}
				return parsed.Entries, nil
			}
		}
	}
	return nil, fmt.Errorf("no output_text in response: %s", body)
}

var weekdays = [...]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}
