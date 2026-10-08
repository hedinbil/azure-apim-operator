// Package apim provides functions for interacting with Azure API Management (APIM) REST API.
// This file reads the little the operator needs to know about an OpenAPI document: whether
// it is JSON or YAML, and whether it declares an OpenAPI 3 or a Swagger 2 version.
package apim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// ErrNotOpenAPIDocument marks a document without a top-level openapi or swagger field.
var ErrNotOpenAPIDocument = errors.New("not an OpenAPI document: no top-level openapi or swagger field")

// ErrUnsupportedDocument marks a document this operator will not send to APIM in the form it
// was served, however often it tries. Test with errors.Is.
var ErrUnsupportedDocument = errors.New("OpenAPI document not supported in this form")

// maxFullYAMLParse bounds the YAML documents parsed in full. Parsing builds the whole
// document in memory at roughly a hundred times its size, so a large one would take the
// operator over its memory limit; only an unusual small document needs it.
const maxFullYAMLParse = 256 << 10

// documentVersion says which version field a document declares at the top level.
type documentVersion int

const (
	versionNone    documentVersion = iota // neither field
	versionOpenAPI                        // openapi: an OpenAPI 3 document
	versionSwagger                        // swagger: a Swagger 2.0 document
)

// documentInfo is what inspectDocument learned about a document.
type documentInfo struct {
	json    bool
	version documentVersion
}

// inspectDocument tells JSON from YAML and finds the top-level version field without
// building the document in memory. Decoding a document of several megabytes into a map, as
// this operator used to on every fetch and again on every import, allocates hundreds of
// megabytes, against a memory limit of 256 MiB shared by four concurrent reconciles.
//
// JSON is decoded into a struct with only the two fields, which encoding/json does without
// keeping anything else. YAML is read line by line for a top-level key; only a small
// document whose top level is not a plain block mapping (a flow mapping, a sequence, a
// broken document) is parsed in full, to say what it is. A large one is refused.
func inspectDocument(body []byte) (documentInfo, error) {
	if json.Valid(body) {
		var top struct {
			OpenAPI json.RawMessage `json:"openapi"`
			Swagger json.RawMessage `json:"swagger"`
		}
		if err := json.Unmarshal(body, &top); err != nil {
			return documentInfo{}, fmt.Errorf("document is not a JSON object: %w", err)
		}
		// encoding/json matches field names regardless of case; only the exact key counts.
		info := documentInfo{json: true}
		switch {
		case top.Swagger != nil && bytes.Contains(body, []byte(`"swagger"`)):
			info.version = versionSwagger
		case top.OpenAPI != nil && bytes.Contains(body, []byte(`"openapi"`)):
			info.version = versionOpenAPI
		}
		return info, nil
	}

	if version := yamlTopLevelVersion(body); version != versionNone {
		return documentInfo{version: version}, nil
	}
	if len(body) > maxFullYAMLParse {
		return documentInfo{}, fmt.Errorf("YAML document of %d bytes without a top-level openapi or swagger key at the start of a line: %w",
			len(body), ErrNotOpenAPIDocument)
	}
	var top struct {
		OpenAPI any `yaml:"openapi"`
		Swagger any `yaml:"swagger"`
	}
	if err := yaml.Unmarshal(body, &top); err != nil {
		return documentInfo{}, fmt.Errorf("document is neither JSON nor a YAML mapping: %w", err)
	}
	info := documentInfo{}
	switch {
	case top.Swagger != nil:
		info.version = versionSwagger
	case top.OpenAPI != nil:
		info.version = versionOpenAPI
	}
	return info, nil
}

// yamlTopLevelVersion finds a top-level openapi or swagger key with a value in the first
// document of a block-style YAML stream. A key at column 0 is a top-level key: everything
// nested under one, block scalars included, has to be indented. It answers versionNone
// when unsure; the caller then parses the document in full or refuses it.
func yamlTopLevelVersion(body []byte) documentVersion {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), len(body)+1)
	scanner.Split(scanYAMLLines)
	found := versionNone
	seenContent := false
	for scanner.Scan() {
		// Bytes, not Text: Text copies every line of a document that can be megabytes.
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == '#' || line[0] == '%' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !seenContent && len(bytes.TrimSpace(line)) > 0 && bytes.TrimSpace(line)[0] != '#' {
				// The document's top level itself is indented, so column 0 is not the top
				// level and a key found there may not belong to the document at all.
				return versionNone
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("---")) || bytes.HasPrefix(line, []byte("...")) {
			if seenContent {
				break // only the first document counts
			}
			continue
		}
		seenContent = true
		key, value, ok := yamlKey(line)
		if !ok || len(value) == 0 {
			continue
		}
		switch key {
		case "swagger":
			return versionSwagger
		case "openapi":
			found = versionOpenAPI
		}
	}
	return found
}

// scanYAMLLines splits at \n, \r\n and a lone \r, all of which end a line in YAML.
func scanYAMLLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if !atEOF {
				return 0, nil, nil // a \n may follow
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// yamlKey splits a top-level "key: value" line. A plain key ends at the first ": " (or a
// colon ending the line); a quoted key ends at its closing quote, which a colon must follow.
// value is what follows the colon, without a trailing comment. ok is false for any other
// shape of line.
func yamlKey(line []byte) (key string, value []byte, ok bool) {
	var rest []byte
	if quote := line[0]; quote == '"' || quote == '\'' {
		end := bytes.IndexByte(line[1:], quote)
		if end < 0 {
			return "", nil, false
		}
		key, rest = string(line[1:1+end]), line[2+end:]
		rest = bytes.TrimLeft(rest, " \t")
		if len(rest) == 0 || rest[0] != ':' {
			return "", nil, false
		}
		rest = rest[1:]
	} else {
		i := bytes.Index(line, []byte(": "))
		switch {
		case i >= 0:
			key, rest = string(line[:i]), line[i+2:]
		case bytes.HasSuffix(line, []byte(":")):
			key, rest = string(line[:len(line)-1]), nil
		default:
			return "", nil, false
		}
		key = strings.TrimRight(key, " \t")
	}
	if i := bytes.Index(rest, []byte(" #")); i >= 0 {
		rest = rest[:i]
	}
	value = bytes.TrimSpace(rest)
	if string(value) == "~" || string(value) == "null" {
		value = nil
	}
	return key, value, true
}

// ValidateOpenAPIDocument checks that body is a JSON object or a YAML mapping with a
// top-level openapi or swagger field, the least a document must have before it is sent to
// APIM. APIM checks the rest.
func ValidateOpenAPIDocument(body []byte) error {
	info, err := inspectDocument(body)
	if err != nil {
		return err
	}
	if info.version == versionNone {
		return ErrNotOpenAPIDocument
	}
	return nil
}
