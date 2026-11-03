// cmd/example/main.go — Example usage of the gnvipi Go client.
//
// Usage:
//
//	# Captcha token mode (reverse-engineered)
//	go run . -captcha "P1_..."
//
//	# Auto-extract captcha token via chromedp (no manual token needed)
//	go run . -auto
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"
	"unicode/utf8"

	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/gnvipi"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	captchaFlag := flag.String("captcha", "", "hCaptcha token (reverse-engineered mode)")
	auto := flag.Bool("auto", false, "Auto-extract captcha token via chromedp")
	model := flag.String("model", "", "model id (e.g. moonshotai/kimi-k3); empty = "+gnvipi.DefaultModel)
	prompt := flag.String("prompt", "Explain the meaning of life in one sentence.", "prompt to send")
	stream := flag.Bool("stream", true, "use streaming")
	smoothMs := flag.Int("smooth-ms", 12, "typewriter delay per rune for stream output (0=off)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// --- Build client (model + captcha from flags) ---

	var token string
	switch {
	case *auto:
		t, err := extractCaptchaToken(ctx)
		if err != nil {
			return fmt.Errorf("failed to auto-extract captcha token: %w", err)
		}
		token = t
		fmt.Printf("✓ Extracted captcha token: %s...\n", tokenPreview(token))

	case *captchaFlag != "":
		token = *captchaFlag

	default:
		return fmt.Errorf("specify -captcha or -auto")
	}

	opts := []gnvipi.Option{gnvipi.WithCaptchaToken(token)}
	if *model != "" {
		opts = append(opts, gnvipi.WithModel(*model))
	}
	client := gnvipi.New(opts...)

	// --- Send request ---
	messages := []gnvipi.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: *prompt},
	}

	if *stream {
		fmt.Print("\n=== Streaming response ===\n\n")
		smooth := time.Duration(*smoothMs) * time.Millisecond
		var lastUsage *gnvipi.Usage
		var reasoningStarted, contentStarted bool
		err := client.StreamChat(ctx, messages, func(chunk gnvipi.StreamChunk) {
			if chunk.Error != nil {
				log.Printf("Stream error: %v", chunk.Error)
				return
			}
			if chunk.Reasoning != "" {
				if !reasoningStarted {
					fmt.Print("--- thinking ---\n")
					reasoningStarted = true
				}
				writeSmooth(chunk.Reasoning, smooth)
			}
			if chunk.Content != "" {
				if reasoningStarted && !contentStarted {
					fmt.Print("\n--- answer ---\n")
					contentStarted = true
				}
				writeSmooth(chunk.Content, smooth)
			}
			if chunk.Usage != nil {
				lastUsage = chunk.Usage
			}
		})
		if err != nil {
			return fmt.Errorf("stream chat failed: %w", err)
		}
		fmt.Println()
		if lastUsage != nil {
			fmt.Printf("\n[Usage: %s]\n", lastUsage.Format())
		}
	} else {
		resp, err := client.Chat(ctx, messages)
		if err != nil {
			return fmt.Errorf("chat failed: %w", err)
		}
		fmt.Print("\n=== Response ===\n\n")
		if len(resp.Choices) > 0 {
			msg := resp.Choices[0].Message
			if msg.ReasoningContent != "" {
				fmt.Printf("--- thinking ---\n%s\n\n--- answer ---\n", msg.ReasoningContent)
			}
			fmt.Println(msg.Content)
		}
		if resp.Usage != nil {
			fmt.Printf("\n[Usage: %s]\n", resp.Usage.Format())
		}
	}
	return nil
}

// writeSmooth prints s rune-by-rune with a fixed delay. delay<=0 prints at once.
func writeSmooth(s string, delay time.Duration) {
	if delay <= 0 || s == "" {
		fmt.Print(s)
		return
	}
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		fmt.Printf("%c", r)
		s = s[size:]
		time.Sleep(delay)
	}
}

// tokenPreview shortens a token for diagnostic output without panicking on
// tokens shorter than the preview length.
func tokenPreview(token string) string {
	if len(token) > 30 {
		return token[:30]
	}
	return token
}

// extractCaptchaToken loads the NVIDIA Playground, triggers hCaptcha, and
// extracts the token from data-hcaptcha-response.
func extractCaptchaToken(baseCtx context.Context) (string, error) {
	return captcha.Extract(baseCtx)
}
