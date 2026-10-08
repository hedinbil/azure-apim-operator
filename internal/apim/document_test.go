package apim

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInspectDocument pins what the operator reads from a document: JSON or YAML, and which
// top-level version field it declares. Only the top level counts; a nested openapi key, a
// commented one or an indented one does not make a document OpenAPI.
func TestInspectDocument(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantJSON bool
		want     documentVersion
	}{
		// JSON.
		{"json openapi", `{"openapi":"3.0.2","info":{"title":"x"}}`, true, versionOpenAPI},
		{"json openapi 3.1 last", `{"info":{},"paths":{},"openapi":"3.1.0"}`, true, versionOpenAPI},
		{"json swagger", `{"swagger":"2.0","host":"svc.cluster.local"}`, true, versionSwagger},
		{"json swagger wins over openapi", `{"openapi":"3.0.0","swagger":"2.0"}`, true, versionSwagger},
		{"json neither", `{"info":{"title":"x"}}`, true, versionNone},
		{"json empty object", `{}`, true, versionNone},
		{"json nested openapi does not count", `{"info":{"openapi":"3.0.0"},"components":{"swagger":"2.0"}}`, true, versionNone},
		{"json with whitespace", " \n\t{ \"openapi\" : \"3.0.0\" }\n", true, versionOpenAPI},
		// JSON-F1: keys are compared exactly, although encoding/json matches them in any case.
		{"F1 json key in another case", `{"OpenAPI":"3.0.0"}`, true, versionNone},
		{"F1 json swagger in another case", `{"SWAGGER":"2.0"}`, true, versionNone},
		{"F1 json Swagger beside openapi", `{"Swagger":"2.0","openapi":"3.0.0"}`, true, versionOpenAPI},
		{"F1 json OpenAPI beside swagger", `{"OpenAPI":"3.0.0","swagger":"2.0"}`, true, versionSwagger},

		// Block YAML.
		{"yaml openapi", "openapi: 3.0.0\ninfo:\n  title: x\n", false, versionOpenAPI},
		{"yaml openapi not first", "info:\n  title: x\npaths: {}\nopenapi: 3.1.0\n", false, versionOpenAPI},
		{"yaml swagger", "swagger: '2.0'\nbasePath: /v1\n", false, versionSwagger},
		{"yaml swagger wins over an earlier openapi", "openapi: 3.0.0\nswagger: '2.0'\n", false, versionSwagger},
		{"yaml double-quoted key", "\"openapi\": \"3.0.0\"\ninfo: {}\n", false, versionOpenAPI},
		{"yaml single-quoted key", "'swagger': '2.0'\ninfo: {}\n", false, versionSwagger},
		{"yaml space before the colon", "openapi : 3.0.0\n", false, versionOpenAPI},
		{"yaml document start marker", "---\nopenapi: 3.0.0\n", false, versionOpenAPI},
		{"yaml directive and marker", "%YAML 1.2\n---\nswagger: '2.0'\n", false, versionSwagger},
		{"yaml CRLF line endings", "openapi: 3.0.0\r\ninfo:\r\n  title: x\r\n", false, versionOpenAPI},
		{"yaml comment does not count", "# openapi: 3.0.0\ninfo:\n  title: x\n", false, versionNone},
		{"yaml comment does not count, a real key later does", "#swagger: '2.0'\nopenapi: 3.0.0\n", false, versionOpenAPI},
		{"yaml indented nested key does not count", "info:\n  openapi: 3.0.0\n  swagger: '2.0'\n", false, versionNone},
		{"yaml block scalar content does not count", "info:\n  description: |\n    openapi: 3.0.0\n    swagger: 2.0\n", false, versionNone},
		{"yaml lookalike key does not count", "openapi_version: 3.0.0\nx-swagger: 2.0\n", false, versionNone},
		{"yaml comment after the version", "openapi: 3.0.0 # the version\n", false, versionOpenAPI},
		{"yaml empty string version counts", "openapi: \"\"\n", false, versionOpenAPI},

		// Regression cases for the fuzz findings of 2026-10-08 (fuzz_test.go).
		// YAML-F1: a lone CR ends a line.
		{"F1 CR line breaks", "info: x\rswagger: '2.0'\r", false, versionSwagger},
		{"F1 CR line breaks after openapi", "openapi: 3.0.0\rswagger: '2.0'\r", false, versionSwagger},
		{"F1 CR only", "openapi: 3.0.0\rinfo:\r  title: x\r", false, versionOpenAPI},
		{"F1 CR then CRLF", "openapi: 3.0.0\r\r\ninfo: {}\r\n", false, versionOpenAPI},
		// YAML-F2: only the first document counts.
		{"F2 second document after ---", "openapi: 3.0.0\n---\nswagger: '2.0'\n", false, versionOpenAPI},
		{"F2 second document after ...", "openapi: 3.0.0\n...\nswagger: '2.0'\n", false, versionOpenAPI},
		{"F2 second document with a comment on the marker", "openapi: 3.0.0\n--- # next\nswagger: '2.0'\n", false, versionOpenAPI},
		{"F2 markers before the content", "---\n# c\nswagger: '2.0'\n", false, versionSwagger},
		// YAML-F3: a colon inside a key does not end it.
		{"F3 quoted key with a colon", "\"swagger:x\": 1\nopenapi: 3.0.0\n", false, versionOpenAPI},
		{"F3 plain key with a colon", "swagger:v2: true\nopenapi: 3.0.0\n", false, versionOpenAPI},
		{"F3 single-quoted key with a colon", "'openapi:x': 1\nswagger: '2.0'\n", false, versionSwagger},
		// YAML-F5: a null version is no version.
		{"F5 key without a value", "openapi:\n", false, versionNone},
		{"F5 tilde", "openapi: ~\n", false, versionNone},
		{"F5 null", "openapi: null\n", false, versionNone},
		{"F5 null with a comment", "openapi: ~ # unset\n", false, versionNone},
		{"F5 null swagger, real openapi", "swagger: ~\nopenapi: 3.0.0\n", false, versionOpenAPI},
		{"F5 null openapi, real swagger", "openapi:\nswagger: '2.0'\n", false, versionSwagger},
		{"F5 indented null (fuzz corpus)", " openapi:", false, versionNone},
		{"F5 flow null", "{openapi: }\n", false, versionNone},
		// YAML-F6: quotes only delimit a whole key.
		{"F6 stray double quote (fuzz corpus)", "0: 0\n\nopenapi\": ", false, versionNone},
		{"F6 stray quote in a plain key", "openapi\": 3.0.0\nx: 1\n", false, versionNone},
		{"F6 mismatched quotes", "\"openapi'\": 3.0.0\n", false, versionNone},

		// Flow-style YAML falls back to the full parse.
		{"yaml flow mapping openapi", "{openapi: 3.0.0, paths: {}}\n", false, versionOpenAPI},
		{"yaml flow mapping swagger", "{swagger: '2.0', host: svc}\n", false, versionSwagger},
		{"yaml flow mapping over several lines", "{\n  openapi: 3.0.0,\n  paths: {}\n}\n", false, versionOpenAPI},
		{"yaml flow mapping with a nested openapi", "{info: {openapi: 3.0.0}}\n", false, versionNone},
		{"json-like yaml with a trailing comma", "{\"openapi\": \"3.0.0\",}\n", false, versionOpenAPI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := inspectDocument([]byte(tc.doc))
			if err != nil {
				t.Fatalf("inspectDocument(%q) error = %v", tc.doc, err)
			}
			if info.json != tc.wantJSON || info.version != tc.want {
				t.Errorf("inspectDocument(%q) = %+v, want json=%v version=%v", tc.doc, info, tc.wantJSON, tc.want)
			}
		})
	}
}

// TestInspectDocumentRejectsNonMappings: a document whose top level is not an object or a
// mapping cannot be an OpenAPI document, nor be carried by the import envelope.
func TestInspectDocumentRejectsNonMappings(t *testing.T) {
	for _, doc := range []string{
		`[1,2]`,
		`[{"openapi":"3.0.0"}]`,
		`"openapi"`,
		`42`,
		`true`,
		"- a\n- b\n",
		"- openapi: 3.0.0\n",
		"[openapi, swagger]\n",
		"key: [unclosed\n",
		"a: b: c\n",
		"just a scalar\n",
	} {
		t.Run(doc, func(t *testing.T) {
			if info, err := inspectDocument([]byte(doc)); err == nil {
				t.Errorf("inspectDocument(%q) = %+v, want an error", doc, info)
			}
			if err := ValidateOpenAPIDocument([]byte(doc)); err == nil {
				t.Errorf("ValidateOpenAPIDocument(%q) = nil, want an error", doc)
			}
		})
	}
}

func TestValidateOpenAPIDocument(t *testing.T) {
	valid := []string{
		`{"openapi":"3.0.0"}`,
		`{"swagger":"2.0"}`,
		"openapi: 3.0.0\n",
		"swagger: '2.0'\n",
		"'openapi': 3.1.0\n",
		"{openapi: 3.0.0}\n",
	}
	for _, doc := range valid {
		if err := ValidateOpenAPIDocument([]byte(doc)); err != nil {
			t.Errorf("ValidateOpenAPIDocument(%q) = %v, want nil", doc, err)
		}
	}

	notOpenAPI := []string{
		`{}`,
		`{"info":{"openapi":"3.0.0"}}`,
		"info:\n  openapi: 3.0.0\n",
		"# openapi: 3.0.0\ninfo: {}\n",
		"{info: {swagger: '2.0'}}\n",
		"name: chart\nversion: 1.0.0\n",
		"openapi:\n",
		"openapi: ~\nswagger: null\n",
		`{"OpenAPI":"3.0.0"}`,
	}
	for _, doc := range notOpenAPI {
		if err := ValidateOpenAPIDocument([]byte(doc)); !errors.Is(err, ErrNotOpenAPIDocument) {
			t.Errorf("ValidateOpenAPIDocument(%q) = %v, want ErrNotOpenAPIDocument", doc, err)
		}
	}

	// A parse failure is reported as such, not as a document without a version field.
	if err := ValidateOpenAPIDocument([]byte("key: [unclosed\n")); err == nil || errors.Is(err, ErrNotOpenAPIDocument) {
		t.Errorf("ValidateOpenAPIDocument(broken YAML) = %v, want the parse error", err)
	}
}

func TestYAMLTopLevelVersion(t *testing.T) {
	cases := map[string]documentVersion{
		"":                                   versionNone,
		"openapi: 3.0.0":                     versionOpenAPI, // no trailing newline
		"swagger: \"2.0\"":                   versionSwagger,
		"  openapi: 3.0.0\n":                 versionNone,
		"\topenapi: 3.0.0\n":                 versionNone,
		"#openapi: 3.0.0\n":                  versionNone,
		"openapi\n":                          versionNone, // no colon, no key
		"{openapi: 3.0.0}\n":                 versionNone, // flow style is left to the full parse
		"x: 1\n\nopenapi: 3.0.0\n":           versionOpenAPI,
		"openapi: 3.0.0\npaths:\n  /a: {}\n": versionOpenAPI,
		"openapi:\n":                         versionNone, // no value (YAML-F5)
		"openapi: null # x\n":                versionNone,
		"%YAML 1.2\nopenapi: 3.0.0\n":        versionOpenAPI,
		"openapi: 3.0.0\r":                   versionOpenAPI, // a lone CR at the end (YAML-F1)
		"x: 1\ropenapi: 3.0.0":               versionOpenAPI,
		"x: 1\n---\nopenapi: 3.0.0\n":        versionNone, // second document (YAML-F2)
		"x: 1\n...\nswagger: '2.0'\n":        versionNone,
		"swagger:x: 1\n":                     versionNone, // colon in the key (YAML-F3)
		"'swagger":                           versionNone, // unclosed quote (YAML-F6)
		"\"openapi\"x: 1\n":                  versionNone,
	}
	for doc, want := range cases {
		if got := yamlTopLevelVersion([]byte(doc)); got != want {
			t.Errorf("yamlTopLevelVersion(%q) = %v, want %v", doc, got, want)
		}
	}
}

// TestYAMLTopLevelVersionReadsLongLines: a line longer than bufio.Scanner's default 64 KiB
// limit (a minified example, an embedded description) must not stop the scan before the
// version field.
func TestYAMLTopLevelVersionReadsLongLines(t *testing.T) {
	doc := "description: " + strings.Repeat("x", 200*1024) + "\nopenapi: 3.0.0\n"
	if got := yamlTopLevelVersion([]byte(doc)); got != versionOpenAPI {
		t.Errorf("yamlTopLevelVersion() = %v after a 200 KiB line, want versionOpenAPI", got)
	}
}

// bigJSONDocument builds a JSON OpenAPI document of at least size bytes, with many paths
// and the version field last so the whole document has to be read.
func bigJSONDocument(size int) []byte {
	var b bytes.Buffer
	b.Grow(size + 1024)
	b.WriteString(`{"info":{"title":"big","version":"1.0"},"paths":{`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"/items/%d/{id}":{"get":{"operationId":"get%d","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"200":{"description":"ok %d"}}}}`, i, i, i)
	}
	b.WriteString(`},"openapi":"3.0.1"}`)
	return b.Bytes()
}

// TestInspectDocumentDoesNotBuildTheDocument is the memory guard: inspecting an 8 MB
// document must allocate a small fraction of its size. Decoding it into a map, as the
// operator used to, allocates many times the document; with a 256 MiB limit and four
// concurrent reconciles that is what ran the operator out of memory.
func TestInspectDocumentDoesNotBuildTheDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 8 MB document")
	}
	const size = 8 << 20
	const limit = 1 << 20
	doc := bigJSONDocument(size)

	// Warm up once, so one-time allocations (encoding/json's type cache) do not count.
	if info, err := inspectDocument(doc); err != nil || !info.json || info.version != versionOpenAPI {
		t.Fatalf("inspectDocument() = %+v, %v; want JSON OpenAPI", info, err)
	}

	const runs = 3
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		if _, err := inspectDocument(doc); err != nil {
			t.Fatalf("inspectDocument() error = %v", err)
		}
	}
	runtime.ReadMemStats(&after)
	perRun := (after.TotalAlloc - before.TotalAlloc) / runs
	t.Logf("inspectDocument allocated %d bytes per run for a %d byte document", perRun, len(doc))
	if perRun > limit {
		t.Errorf("inspectDocument allocated %d bytes per run for a %d byte document, want under %d: is it decoding the whole document?",
			perRun, len(doc), limit)
	}

	// The same document through ValidateOpenAPIDocument, which the fetch path calls.
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := ValidateOpenAPIDocument(doc); err != nil {
		t.Fatalf("ValidateOpenAPIDocument() = %v", err)
	}
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Errorf("ValidateOpenAPIDocument allocated %d bytes for a %d byte document, want under %d", got, len(doc), limit)
	}
}

// TestImportFormatSendsLargeJSONUnchanged: the import sends the fetched bytes themselves;
// a JSON document is never re-encoded on its way into the envelope.
func TestImportFormatSendsLargeJSONUnchanged(t *testing.T) {
	doc := bigJSONDocument(1 << 20)
	format, out, err := importFormat(doc)
	if err != nil {
		t.Fatalf("importFormat() error = %v", err)
	}
	if format != "openapi+json" {
		t.Errorf("format = %q, want openapi+json", format)
	}
	if &out[0] != &doc[0] || len(out) != len(doc) {
		t.Error("importFormat() returned a copy, want the document itself")
	}
}

// flowYAMLDocument builds a flow-style YAML mapping of exactly size bytes, over many lines,
// with openapi as its first key. No line has a key at column 0, so the line scan finds
// nothing and only a full parse can tell what the document is.
func flowYAMLDocument(size int) []byte {
	const head, tail = "{\n  openapi: 3.0.0,\n  info: {title: '", "'},\n  paths: {\n"
	const end = "  }\n}\n"
	var paths bytes.Buffer
	for i := 0; ; i++ {
		entry := fmt.Sprintf("    /items/%d: {get: {responses: {'200': {description: ok}}}},\n", i)
		if len(head)+len(tail)+paths.Len()+len(entry)+len(end) > size {
			break
		}
		paths.WriteString(entry)
	}
	title := strings.Repeat("x", size-len(head)-len(tail)-paths.Len()-len(end))
	return []byte(head + title + tail + paths.String() + end)
}

// TestInspectDocumentRefusesLargeFlowYAML: a YAML document without a top-level key at the
// start of a line is parsed in full only up to maxFullYAMLParse; a parse builds it in memory
// at about a hundred times its size. A larger one is refused as not an OpenAPI document, at
// once and without building it.
func TestInspectDocumentRefusesLargeFlowYAML(t *testing.T) {
	// At the limit: parsed in full, and found to be OpenAPI.
	small := flowYAMLDocument(maxFullYAMLParse)
	if len(small) != maxFullYAMLParse {
		t.Fatalf("document is %d bytes, want %d", len(small), maxFullYAMLParse)
	}
	if got := yamlTopLevelVersion(small); got != versionNone {
		t.Fatalf("yamlTopLevelVersion() = %v, want versionNone: the test needs a full parse", got)
	}
	if info, err := inspectDocument(small); err != nil || info.json || info.version != versionOpenAPI {
		t.Errorf("inspectDocument(%d byte flow YAML) = %+v, %v; want YAML OpenAPI", len(small), info, err)
	}

	for _, size := range []int{maxFullYAMLParse + 1, 8 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			doc := flowYAMLDocument(size)
			if _, err := inspectDocument(doc); !errors.Is(err, ErrNotOpenAPIDocument) {
				t.Fatalf("inspectDocument(%d byte flow YAML) = %v, want ErrNotOpenAPIDocument", len(doc), err)
			}
			if err := ValidateOpenAPIDocument(doc); !errors.Is(err, ErrNotOpenAPIDocument) {
				t.Errorf("ValidateOpenAPIDocument() = %v, want ErrNotOpenAPIDocument", err)
			}
			if _, _, err := importFormat(doc); !errors.Is(err, ErrNotOpenAPIDocument) {
				t.Errorf("importFormat() = %v, want ErrNotOpenAPIDocument", err)
			}

			const runs = 3
			// A full parse allocates many times the document; the refusal allocates the line
			// scanner's buffer and the error.
			const limit = 256 << 10
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			start := time.Now()
			for range runs {
				if _, err := inspectDocument(doc); err == nil {
					t.Fatal("inspectDocument() = nil error")
				}
			}
			elapsed := time.Since(start) / runs
			runtime.ReadMemStats(&after)
			perRun := (after.TotalAlloc - before.TotalAlloc) / runs
			t.Logf("refusing a %d byte flow YAML document took %s and allocated %d bytes", len(doc), elapsed, perRun)
			if perRun > limit {
				t.Errorf("refusing a %d byte document allocated %d bytes, want under %d: was it parsed?", len(doc), perRun, limit)
			}
			// Generous for -race and a loaded machine; a full parse of 8 MiB takes seconds.
			if elapsed > 2*time.Second {
				t.Errorf("refusing a %d byte document took %s", len(doc), elapsed)
			}
		})
	}
}
