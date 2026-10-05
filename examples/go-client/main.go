package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Example CLI that drives the pdf2zh_next HTTP API end to end:
//
//	submit a PDF -> follow progress (SSE or polling) -> download results.
//
// Usage:
//
//	go run . -url http://127.0.0.1:11008 -file paper.pdf -lang-in en -lang-out zh
//	go run . -file paper.pdf -engine openai -openai-key sk-... -openai-model gpt-4o-mini
//	go run . -file paper.pdf -poll   # use polling instead of SSE
func main() {
	var (
		baseURL     = flag.String("url", "http://127.0.0.1:11008", "pdf2zh API base URL")
		file        = flag.String("file", "", "path to the PDF to translate (required)")
		langIn      = flag.String("lang-in", "en", "source language")
		langOut     = flag.String("lang-out", "zh", "target language")
		qps         = flag.Int("qps", 4, "QPS limit")
		pages       = flag.String("pages", "", "pages to translate, e.g. 1-5 (empty = all)")
		watermark   = flag.String("watermark", "watermarked", "watermark mode: watermarked|no_watermark|both")
		engine      = flag.String("engine", "google", "translation engine: google|bing|openai|deepl")
		openaiKey   = flag.String("openai-key", "", "OpenAI API key (engine=openai)")
		openaiModel = flag.String("openai-model", "gpt-4o-mini", "OpenAI model (engine=openai)")
		openaiBase  = flag.String("openai-base-url", "", "OpenAI base URL (engine=openai)")
		deeplKey    = flag.String("deepl-key", "", "DeepL auth key (engine=deepl)")
		outDir      = flag.String("out", ".", "output directory for downloaded PDFs")
		poll        = flag.Bool("poll", false, "use polling instead of SSE for progress")
	)
	flag.Parse()

	if *file == "" {
		log.Fatal("-file is required")
	}

	engineSettings, err := buildEngineSettings(*engine, *openaiKey, *openaiModel, *openaiBase, *deeplKey)
	if err != nil {
		log.Fatal(err)
	}

	// Cancel the task if the user interrupts (Ctrl-C).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := NewClient(*baseURL)

	req := TranslateRequest{
		LangIn:              *langIn,
		LangOut:             *langOut,
		QPS:                 *qps,
		Pages:               *pages,
		WatermarkOutputMode: *watermark,
		EngineSettings:      engineSettings,
	}

	fmt.Printf("Submitting %s ...\n", *file)
	taskID, err := client.Submit(ctx, *file, req)
	if err != nil {
		log.Fatalf("submit failed: %v", err)
	}
	fmt.Printf("task id: %s\n", taskID)

	var final *Event
	if *poll {
		final, err = followByPolling(ctx, client, taskID)
	} else {
		final, err = followBySSE(ctx, client, taskID)
	}
	if err != nil {
		// Best-effort cancel so the server frees resources.
		_ = client.Cancel(context.Background(), taskID)
		log.Fatalf("progress failed: %v", err)
	}
	if final != nil && final.Type == "error" {
		log.Fatalf("translation error [%s]: %s", final.ErrorType, final.Error)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if final != nil && final.DualURL != "" {
		dst := filepath.Join(*outDir, taskID+"-dual.pdf")
		if err := client.Download(ctx, taskID, "dual", dst); err != nil {
			log.Fatalf("download dual: %v", err)
		}
		fmt.Printf("saved %s\n", dst)
	}
	if final != nil && final.MonoURL != "" {
		dst := filepath.Join(*outDir, taskID+"-mono.pdf")
		if err := client.Download(ctx, taskID, "mono", dst); err != nil {
			log.Fatalf("download mono: %v", err)
		}
		fmt.Printf("saved %s\n", dst)
	}
	fmt.Println("done")
}

func followBySSE(ctx context.Context, client *Client, taskID string) (*Event, error) {
	var final *Event
	err := client.Stream(ctx, taskID, func(ev Event) {
		switch ev.Type {
		case "finish":
			final = &ev
			fmt.Printf("\rfinished in %.1fs\n", ev.TotalSeconds)
		case "error":
			final = &ev
		default:
			fmt.Printf("\r[%5.1f%%] %s (part %d/%d)        ",
				ev.OverallProgress, ev.Stage, ev.PartIndex, ev.TotalParts)
		}
	})
	return final, err
}

func followByPolling(ctx context.Context, client *Client, taskID string) (*Event, error) {
	st, err := client.Poll(ctx, taskID, 2*time.Second, func(s *Status) {
		fmt.Printf("\r[%5.1f%%] %s (%s)        ", s.Progress, s.Stage, s.State)
	})
	if err != nil {
		return nil, err
	}
	fmt.Println()
	evType := "finish"
	if st.State != "finished" {
		evType = "error" // error or cancelled
	}
	return &Event{
		Type:         evType,
		TotalSeconds: st.TotalSeconds,
		MonoURL:      st.MonoURL,
		DualURL:      st.DualURL,
		Error:        st.Error,
		ErrorType:    st.ErrorType,
	}, nil
}

func buildEngineSettings(engine, openaiKey, openaiModel, openaiBase, deeplKey string) (map[string]any, error) {
	switch engine {
	case "google":
		return map[string]any{"translate_engine_type": "Google"}, nil
	case "bing":
		return map[string]any{"translate_engine_type": "Bing"}, nil
	case "openai":
		if openaiKey == "" {
			return nil, fmt.Errorf("engine=openai requires -openai-key")
		}
		s := map[string]any{
			"translate_engine_type": "OpenAI",
			"openai_api_key":        openaiKey,
			"openai_model":          openaiModel,
		}
		if openaiBase != "" {
			s["openai_base_url"] = openaiBase
		}
		return s, nil
	case "deepl":
		if deeplKey == "" {
			return nil, fmt.Errorf("engine=deepl requires -deepl-key")
		}
		return map[string]any{
			"translate_engine_type": "DeepL",
			"deepl_auth_key":        deeplKey,
		}, nil
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
}
