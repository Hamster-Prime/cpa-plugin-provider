package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func rewriteResponseModel(payload []byte, publicModel string, enabled bool) []byte {
	if !enabled || strings.TrimSpace(publicModel) == "" || !gjson.ValidBytes(payload) {
		return append([]byte(nil), payload...)
	}
	return rewriteJSONModel(payload, publicModel)
}

func rewriteJSONModel(payload []byte, publicModel string) []byte {
	updated := append([]byte(nil), payload...)
	for _, path := range []string{"model", "modelVersion", "response.model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		if next, err := sjson.SetBytes(updated, path, publicModel); err == nil {
			updated = next
		}
	}
	return updated
}

func rewriteStreamPayload(payload []byte, publicModel string, enabled bool) []byte {
	if !enabled || strings.TrimSpace(publicModel) == "" || len(payload) == 0 {
		return append([]byte(nil), payload...)
	}
	if gjson.ValidBytes(bytes.TrimSpace(payload)) {
		return rewriteJSONModel(payload, publicModel)
	}

	lines := bytes.SplitAfter(payload, []byte("\n"))
	for index, line := range lines {
		ending := []byte(nil)
		content := line
		if bytes.HasSuffix(content, []byte("\n")) {
			ending = []byte("\n")
			content = bytes.TrimSuffix(content, ending)
		}
		trimmed := bytes.TrimSpace(content)
		prefix := []byte("data:")
		if !bytes.HasPrefix(trimmed, prefix) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len(prefix):])
		if !gjson.ValidBytes(data) {
			continue
		}
		leadingLength := len(content) - len(bytes.TrimLeft(content, " \t\r"))
		leading := content[:leadingLength]
		mapped := rewriteJSONModel(data, publicModel)
		lines[index] = append(append(append(append([]byte(nil), leading...), prefix...), ' '), mapped...)
		lines[index] = append(lines[index], ending...)
	}
	return bytes.Join(lines, nil)
}

func convertHTTPChunks(ctx context.Context, input <-chan pluginapi.HTTPStreamChunk, publicModel string, forceMapping bool) <-chan pluginapi.ExecutorStreamChunk {
	return convertHTTPChunksWithClose(ctx, input, publicModel, forceMapping, false, nil)
}

func convertHTTPChunksWithClose(ctx context.Context, input <-chan pluginapi.HTTPStreamChunk, publicModel string, forceMapping, stripSSE bool, onClose func()) <-chan pluginapi.ExecutorStreamChunk {
	if stripSSE {
		return convertGeminiSSEChunksWithClose(ctx, input, publicModel, forceMapping, onClose)
	}
	output := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(output)
		if onClose != nil {
			defer onClose()
		}
		pending := make([]byte, 0)
		emit := func(payload []byte, err error) bool {
			if len(payload) == 0 && err == nil {
				return true
			}
			select {
			case output <- pluginapi.ExecutorStreamChunk{Payload: payload, Err: err}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emitRecord := func(payload []byte) bool {
			if len(payload) == 0 {
				return true
			}
			mapped := rewriteStreamPayload(payload, publicModel, forceMapping)
			return emit(mapped, nil)
		}
		flush := func() bool {
			if len(pending) == 0 {
				return true
			}
			payload := append([]byte(nil), pending...)
			pending = pending[:0]
			return emitRecord(payload)
		}
		streamTooLarge := func() bool {
			return emit(nil, fmt.Errorf("SSE record exceeds %d bytes", maxErrorBodyBytes))
		}
		processPending := func() bool {
			for {
				newline := bytes.IndexByte(pending, '\n')
				if newline < 0 {
					break
				}
				if newline+1 > maxErrorBodyBytes {
					streamTooLarge()
					return false
				}
				line := append([]byte(nil), pending[:newline+1]...)
				pending = append(pending[:0], pending[newline+1:]...)
				if !emitRecord(line) {
					return false
				}
			}
			if len(pending) > maxErrorBodyBytes {
				streamTooLarge()
				return false
			}
			if completeStreamRecord(pending) {
				return flush()
			}
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-input:
				if !ok {
					flush()
					return
				}
				pending = append(pending, chunk.Payload...)
				if !processPending() {
					return
				}
				if chunk.Err != nil {
					if !flush() {
						return
					}
					emit(nil, chunk.Err)
					return
				}
			}
		}
	}()
	return output
}

func convertGeminiSSEChunksWithClose(ctx context.Context, input <-chan pluginapi.HTTPStreamChunk, publicModel string, forceMapping bool, onClose func()) <-chan pluginapi.ExecutorStreamChunk {
	output := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(output)
		if onClose != nil {
			defer onClose()
		}

		pending := make([]byte, 0)
		dataLines := make([][]byte, 0, 1)
		eventBytes := 0
		emit := func(payload []byte, err error) bool {
			if len(payload) == 0 && err == nil {
				return true
			}
			select {
			case output <- pluginapi.ExecutorStreamChunk{Payload: payload, Err: err}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emitEvent := func() bool {
			if len(dataLines) == 0 {
				return true
			}
			data := bytes.Join(dataLines, []byte("\n"))
			dataLines = dataLines[:0]
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				return true
			}
			if !gjson.ValidBytes(data) {
				emit(nil, fmt.Errorf("invalid Gemini SSE data JSON"))
				return false
			}
			return emit(rewriteResponseModel(data, publicModel, forceMapping), nil)
		}
		processLine := func(line []byte) bool {
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) == 0 {
				return emitEvent()
			}
			if line[0] == ':' {
				return true
			}
			field, value, found := bytes.Cut(line, []byte(":"))
			if !found {
				value = nil
			}
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if bytes.Equal(field, []byte("data")) {
				dataLines = append(dataLines, append([]byte(nil), value...))
			}
			return true
		}
		streamTooLarge := func() bool {
			return emit(nil, fmt.Errorf("Gemini SSE event exceeds %d bytes", maxErrorBodyBytes))
		}

		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-input:
				if !ok {
					if len(pending) > 0 {
						eventBytes += len(pending)
						if eventBytes > maxErrorBodyBytes {
							streamTooLarge()
							return
						}
						if !processLine(pending) {
							return
						}
					}
					emitEvent()
					return
				}

				pending = append(pending, chunk.Payload...)
				for {
					newline := bytes.IndexByte(pending, '\n')
					if newline < 0 {
						break
					}
					eventBytes += newline + 1
					if eventBytes > maxErrorBodyBytes {
						streamTooLarge()
						return
					}
					line := append([]byte(nil), pending[:newline]...)
					pending = append(pending[:0], pending[newline+1:]...)
					blank := len(bytes.TrimSuffix(line, []byte("\r"))) == 0
					if !processLine(line) {
						return
					}
					if blank {
						eventBytes = 0
					}
				}
				if eventBytes+len(pending) > maxErrorBodyBytes {
					streamTooLarge()
					return
				}
				if chunk.Err != nil {
					emit(nil, chunk.Err)
					return
				}
			}
		}
	}()
	return output
}

func completeStreamRecord(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	if gjson.ValidBytes(trimmed) {
		return true
	}
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return false
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	return bytes.Equal(data, []byte("[DONE]")) || gjson.ValidBytes(data)
}

func readStreamErrorBody(ctx context.Context, chunks <-chan pluginapi.HTTPStreamChunk) []byte {
	if chunks == nil {
		return nil
	}
	body := make([]byte, 0)
	for len(body) < maxErrorBodyBytes {
		select {
		case <-ctx.Done():
			return body
		case chunk, ok := <-chunks:
			if !ok {
				return body
			}
			if len(chunk.Payload) > 0 {
				remaining := maxErrorBodyBytes - len(body)
				if len(chunk.Payload) > remaining {
					body = append(body, chunk.Payload[:remaining]...)
					return body
				}
				body = append(body, chunk.Payload...)
			}
			if chunk.Err != nil {
				return body
			}
		}
	}
	return body
}
