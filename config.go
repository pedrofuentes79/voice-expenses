package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Config is data/config.json, generated from a Kesef backup by prototype/import_kesef.py.
type Config struct {
	Categories           []string          `json:"expense_categories"`
	CategoryDescriptions map[string]string `json:"category_descriptions"`
	PaymentMethods       []string          `json:"payment_methods"`
	Examples             []struct {
		Note          string  `json:"note"`
		Kind          string  `json:"kind"`
		Amount        float64 `json:"amount"`
		Currency      string  `json:"currency"`
		Category      string  `json:"category"`
		PaymentMethod string  `json:"payment_method"`
	} `json:"examples"`

	termsPath string
}

func LoadConfig(dataDir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	cfg.termsPath = filepath.Join(dataDir, "terms.txt")
	return &cfg, nil
}

// Terms are transcription hints: terms.txt (re-read each call, so edits need no restart) plus payment methods.
func (c *Config) Terms() []string {
	var terms []string
	if data, err := os.ReadFile(c.termsPath); err == nil {
		for line := range strings.Lines(string(data)) {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				terms = append(terms, t)
			}
		}
	}
	for _, m := range c.PaymentMethods {
		if !slices.Contains(terms, m) {
			terms = append(terms, m)
		}
	}
	return terms
}

func (c *Config) Instructions() string {
	var b strings.Builder
	b.WriteString(`Convertí la transcripción de un mensaje de voz en gastos.

- Un mensaje puede tener varios gastos; devolvé uno por cada gasto.
- Si no hay ningún monto, devolvé una lista vacía.
- Montos en jerga argentina: "lucas"/"luquitas" = miles, "palo" = millón, "k" = miles, "mangos"/"pesos" = ARS. "70 lucas" = 70000.
- Moneda ARS salvo que diga dólares/USD/verdes.
- Categorías:
`)
	for _, cat := range c.Categories {
		if d, ok := c.CategoryDescriptions[cat]; ok {
			fmt.Fprintf(&b, "  - %s: %s\n", cat, d)
		} else {
			fmt.Fprintf(&b, "  - %s\n", cat)
		}
	}
	fmt.Fprintf(&b, `- Si ninguna categoría encaja con claridad, category = null y el usuario la elige.
- Medios de pago: %s. "efectivo" = Cash. La transcripción puede deformar el nombre: elegí el más parecido. null si no se menciona.
- La transcripción puede tener errores en nombres propios: copiá el nombre del comercio tal cual se transcribió, no lo inventes ni lo corrijas.
- "merchant": el comercio o lugar donde se gastó, nunca una persona. null si no se menciona.
- "note": descripción corta en español. Si el gasto fue con alguien, nombralo; no repitas el monto ni el medio de pago.
- Fecha: hoy salvo que diga "ayer", un día de la semana, etc. Formato YYYY-MM-DD.
`, strings.Join(c.PaymentMethods, ", "))
	if len(c.Examples) > 0 {
		b.WriteString("\nGastos que el usuario ya cargó, para que imites cómo categoriza:\n")
		for _, e := range c.Examples {
			if e.Kind == "expense" {
				fmt.Fprintf(&b, "- %q → %s, %s, %g %s\n", e.Note, e.Category, e.PaymentMethod, e.Amount, e.Currency)
			}
		}
	}
	return b.String()
}
