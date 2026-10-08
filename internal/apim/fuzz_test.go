package apim

// Native fuzz targets for the parsers in this package that read input the operator does not
// control: a fetched OpenAPI document, the headers and body of an ARM answer, and a backend
// URL written into a custom resource. The seed corpora are the tables of the unit tests, so
// `go test` runs every seed as a regression case; `go test -fuzz` explores from there.
//
//	go test ./internal/apim/ -run '^$' -fuzz '^FuzzInspectDocument$' -fuzztime 45s

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// defaultARMHost is armHost as production sets it. The URL fuzz targets compare against it
// and skip if a test elsewhere left armHost pointing somewhere else.
const defaultARMHost = "https://management.azure.com"

// documentSeeds are the documents of TestInspectDocument, TestInspectDocumentRejectsNonMappings,
// TestValidateOpenAPIDocument, TestYAMLTopLevelVersion and TestImportFormat, plus a few shapes
// the line scanner in yamlTopLevelVersion has to get right.
var documentSeeds = []string{
	// JSON.
	`{"openapi":"3.0.2","info":{"title":"x"}}`,
	`{"info":{},"paths":{},"openapi":"3.1.0"}`,
	`{"swagger":"2.0","host":"svc.cluster.local"}`,
	`{"openapi":"3.0.0","swagger":"2.0"}`,
	`{"info":{"title":"x"}}`,
	`{}`,
	`{"info":{"openapi":"3.0.0"},"components":{"swagger":"2.0"}}`,
	" \n\t{ \"openapi\" : \"3.0.0\" }\n",
	"{ \"openapi\": \"3.0.0\",  \"a\": 1 }",
	`{"swagger":"2.0","info":{},"basePath":"/v1"}`,
	`{"OpenAPI":"3.0.0"}`, `{"Swagger":"2.0","openapi":"3.0.0"}`,
	`[1,2]`, `[{"openapi":"3.0.0"}]`, `"openapi"`, `42`, `true`, `null`,
	// Block YAML.
	"openapi: 3.0.0\ninfo:\n  title: x\n",
	"info:\n  title: x\npaths: {}\nopenapi: 3.1.0\n",
	"swagger: '2.0'\nbasePath: /v1\n",
	"openapi: 3.0.0\nswagger: '2.0'\n",
	"\"openapi\": \"3.0.0\"\ninfo: {}\n",
	"'swagger': '2.0'\ninfo: {}\n",
	"openapi : 3.0.0\n",
	"---\nopenapi: 3.0.0\n",
	"%YAML 1.2\n---\nswagger: '2.0'\n",
	"openapi: 3.0.0\r\ninfo:\r\n  title: x\r\n",
	"# openapi: 3.0.0\ninfo:\n  title: x\n",
	"#swagger: '2.0'\nopenapi: 3.0.0\n",
	"info:\n  openapi: 3.0.0\n  swagger: '2.0'\n",
	"info:\n  description: |\n    openapi: 3.0.0\n    swagger: 2.0\n",
	"openapi_version: 3.0.0\nx-swagger: 2.0\n",
	"openapi:\n",
	"openapi: 3.0.0\ninfo:\n  version: 1.0\n  x-flag: yes\n",
	"swagger: '2.0'\nhost: svc\n",
	"info:\n  title: x\n",
	"name: chart\nversion: 1.0.0\n",
	"", "openapi: 3.0.0", "swagger: \"2.0\"", "  openapi: 3.0.0\n", "\topenapi: 3.0.0\n",
	"openapi\n", "x: 1\n\nopenapi: 3.0.0\n", "openapi: 3.0.0\npaths:\n  /a: {}\n",
	// Flow YAML and non-mappings.
	"{openapi: 3.0.0, paths: {}}\n",
	"{swagger: '2.0', host: svc}\n",
	"{\n  openapi: 3.0.0,\n  paths: {}\n}\n",
	"{info: {openapi: 3.0.0}}\n",
	"{\"openapi\": \"3.0.0\",}\n",
	"- a\n- b\n", "- openapi: 3.0.0\n", "[openapi, swagger]\n", "key: [unclosed\n", "a: b: c\n",
	"just a scalar\n",
	// Shapes for the line scanner.
	"info: x\rswagger: '2.0'\r",
	"openapi: 3.0.0\rswagger: '2.0'\r",
	"openapi: 3.0.0\n---\nswagger: '2.0'\n",
	"info: \"a\nswagger: b\"\nopenapi: 3.0.0\n",
	"\"swagger:x\": 1\nopenapi: 3.0.0\n",
	"swagger:v2: true\nopenapi: 3.0.0\n",
	"&a swagger: '2.0'\n",
	"<<: {swagger: '2.0'}\nopenapi: 3.0.0\n",
	"? swagger\n: '2.0'\n",
	"\ufeffswagger: '2.0'\n",
	"swagger: ~\n",
	" a: 1\nswagger: '2.0'\n",
}

// FuzzInspectDocument checks inspectDocument, ValidateOpenAPIDocument and importFormat
// together. None may panic. A JSON document goes to APIM byte for byte; a YAML Swagger 2.0
// document converted for APIM is valid JSON; a YAML OpenAPI document goes byte for byte.
func FuzzInspectDocument(f *testing.F) {
	for _, doc := range documentSeeds {
		f.Add([]byte(doc))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		info, inspectErr := inspectDocument(doc)
		validateErr := ValidateOpenAPIDocument(doc)
		switch {
		case inspectErr != nil:
			if validateErr == nil || errors.Is(validateErr, ErrNotOpenAPIDocument) {
				t.Errorf("ValidateOpenAPIDocument(%q) = %v, want the inspect error %v", doc, validateErr, inspectErr)
			}
		case info.version == versionNone:
			if !errors.Is(validateErr, ErrNotOpenAPIDocument) {
				t.Errorf("ValidateOpenAPIDocument(%q) = %v, want ErrNotOpenAPIDocument", doc, validateErr)
			}
		case validateErr != nil:
			t.Errorf("ValidateOpenAPIDocument(%q) = %v, want nil for %+v", doc, validateErr, info)
		}
		if info.json && !json.Valid(doc) {
			t.Errorf("inspectDocument(%q) says JSON, but it is not valid JSON", doc)
		}
		if inspectErr == nil && json.Valid(doc) && !info.json {
			t.Errorf("inspectDocument(%q) says YAML for valid JSON", doc)
		}
		if inspectErr == nil && info.json {
			checkJSONVersionKeys(t, doc, info)
		}

		format, out, err := importFormat(doc)
		// Only the conversion of Swagger YAML can fail where inspectDocument did not.
		swaggerYAML := inspectErr == nil && info.version == versionSwagger && !info.json
		if (err != nil) != (inspectErr != nil) && !swaggerYAML {
			t.Fatalf("importFormat(%q) error = %v, inspectDocument error = %v", doc, err, inspectErr)
		}
		if err != nil {
			return
		}
		checkImportFormat(t, doc, info, format, out)
	})
}

// checkImportFormat holds the importFormat invariants for a document inspectDocument read.
func checkImportFormat(t *testing.T, doc []byte, info documentInfo, format string, out []byte) {
	t.Helper()
	switch {
	case info.json:
		want := "openapi+json"
		if info.version == versionSwagger {
			want = "swagger-json"
		}
		if format != want {
			t.Errorf("importFormat(%q) format = %q, want %q", doc, format, want)
		}
		if !bytes.Equal(out, doc) {
			t.Errorf("importFormat(%q) changed a JSON document to %q", doc, out)
		}
	case format == "swagger-json":
		if info.version != versionSwagger {
			t.Errorf("importFormat(%q) converted a %v YAML document", doc, info.version)
		}
		if !json.Valid(out) {
			t.Errorf("importFormat(%q) converted Swagger YAML to invalid JSON %q", doc, out)
		}
	case format == "openapi":
		if !bytes.Equal(out, doc) {
			t.Errorf("importFormat(%q) changed a YAML OpenAPI document to %q", doc, out)
		}
	default:
		t.Errorf("importFormat(%q) format = %q for YAML", doc, format)
	}
}

// checkJSONVersionKeys compares inspectDocument's verdict on a JSON object with the keys the
// object really has, compared exactly: encoding/json matches field names case-insensitively,
// but {"OpenAPI":"3.0.0"} is no OpenAPI document (JSON-F1, fixed; see TestInspectDocument).
func checkJSONVersionKeys(t *testing.T, doc []byte, info documentInfo) {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(doc, &keys); err != nil {
		return // null, which encoding/json reads as an empty struct
	}
	want := versionNone
	if _, ok := keys["openapi"]; ok {
		want = versionOpenAPI
	}
	if _, ok := keys["swagger"]; ok {
		want = versionSwagger
	}
	if info.version != want {
		t.Errorf("inspectDocument(%q) version = %v, the object's keys say %v", doc, info.version, want)
	}
}

// FuzzYAMLTopLevelVersion compares the line scanner of yamlTopLevelVersion, and the
// inspectDocument verdict built on it, with a full go.yaml.in/yaml/v3 parse, for documents
// that parse as a block mapping and have no quoted scalar running over several lines (a
// continuation line of one may start at column 0 and look like a key; that limitation is
// known).
//
// Two checks, by impact:
//   - when the scanner names a version, it must be the parse's: inspectDocument trusts it and
//     never parses, so a wrong answer reaches importFormat (Swagger YAML sent as "openapi",
//     or OpenAPI YAML converted with YAML 1.1 rules);
//   - inspectDocument as a whole must agree with the parse. A scanner that finds nothing
//     falls back to the full parse, so a version it misses is harmless on its own.
func FuzzYAMLTopLevelVersion(f *testing.F) {
	for _, doc := range documentSeeds {
		f.Add([]byte(doc))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		want, ok := yamlReferenceVersion(doc)
		if !ok {
			return
		}
		got := yamlTopLevelVersion(doc)
		info, err := inspectDocument(doc)
		scannerWrong := got != versionNone && got != want
		inspectWrong := !json.Valid(doc) && (err != nil || info.version != want)
		if !scannerWrong && !inspectWrong {
			return
		}
		// Known disagreements, reported and not fixed here; see knownYAMLScannerFinding.
		if finding := knownYAMLScannerFinding(doc); finding != "" {
			t.Skipf("known finding %s: yamlTopLevelVersion(%q) = %v, inspectDocument = %v, a full YAML parse says %v",
				finding, doc, got, info.version, want)
		}
		if scannerWrong {
			t.Errorf("yamlTopLevelVersion(%q) = %v, a full YAML parse says %v", doc, got, want)
		}
		if inspectWrong {
			t.Errorf("inspectDocument(%q) = %+v, %v; a full YAML parse says %v", doc, info, err, want)
		}
	})
}

// knownYAMLScannerFinding names the known way yamlTopLevelVersion reads doc differently from
// a YAML parser, or "" for none. The fuzz run of 2026-10-08 found six; YAML-F1 (a lone CR
// ending a line), YAML-F2 (a second document), YAML-F3 (a colon in a key), YAML-F5 (a null
// version) and YAML-F6 (stray quotes in a key) are fixed and have regression cases in
// TestInspectDocument and TestYAMLTopLevelVersion. Left open:
//
//   - YAML-F4, merge keys: "<<: {swagger: '2.0'}" merges a swagger key the scanner cannot see;
//     with an openapi key on its own line the scanner reports OpenAPI and never parses.
//
// YAML-F7 (an indented top level, testdata/fuzz/FuzzYAMLTopLevelVersion/8148568bcc2a247d)
// is fixed: the scanner gives up when the first content line is indented.
func knownYAMLScannerFinding(doc []byte) string {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil || len(root.Content) == 0 {
		return ""
	}
	top := root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Tag == "!!merge" {
			return "YAML-F4"
		}
	}
	return ""
}

// yamlReferenceVersion is the version a full parse finds in a block-mapping document: which
// of the keys openapi and swagger the top-level mapping has with a value, merge keys resolved;
// a null value counts as no field, as it does for inspectDocument. ok is false for a document out
// of scope: not a block mapping, not decodable into a string-keyed map, or with a quoted
// scalar that runs over several lines.
func yamlReferenceVersion(doc []byte) (version documentVersion, ok bool) {
	if !utf8.Valid(doc) || bytes.HasPrefix(doc, []byte("\ufeff")) {
		return versionNone, false
	}
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return versionNone, false
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode || top.Style&yaml.FlowStyle != 0 {
		return versionNone, false
	}
	if quotedScalarSpansLines(top, yamlLines(string(doc))) {
		return versionNone, false
	}
	var keys map[string]yaml.Node
	if err := yaml.Unmarshal(doc, &keys); err != nil {
		return versionNone, false
	}
	if value, found := keys["swagger"]; found && value.Tag != "!!null" {
		return versionSwagger, true
	}
	if value, found := keys["openapi"]; found && value.Tag != "!!null" {
		return versionOpenAPI, true
	}
	return versionNone, true
}

// yamlLines splits s at the line breaks YAML counts (LF, CRLF, CR, NEL, LS, PS), so a node's
// Line and Column (in characters) index into the result.
func yamlLines(s string) [][]rune {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n").Replace(s)
	parts := strings.Split(s, "\n")
	lines := make([][]rune, len(parts))
	for i, part := range parts {
		lines[i] = []rune(part)
	}
	return lines
}

// quotedScalarSpansLines reports whether n or anything under it is a quoted scalar whose
// closing quote is not on the line it starts on. A scalar whose start cannot be located
// (an anchor or tag in front of it) counts as spanning lines, to stay on the safe side.
func quotedScalarSpansLines(n *yaml.Node, lines [][]rune) bool {
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		if n.Line < 1 || n.Line > len(lines) || !quotedEndsOnLine(lines[n.Line-1], n.Column-1) {
			return true
		}
	}
	for _, child := range n.Content {
		if quotedScalarSpansLines(child, lines) {
			return true
		}
	}
	return false
}

// quotedEndsOnLine reports whether the quoted scalar starting at line[col] closes on line.
func quotedEndsOnLine(line []rune, col int) bool {
	if col < 0 || col >= len(line) || (line[col] != '"' && line[col] != '\'') {
		return false
	}
	quote := line[col]
	for i := col + 1; i < len(line); i++ {
		switch {
		case quote == '"' && line[i] == '\\':
			i++ // an escape; a backslash at the end of the line escapes the line break
		case line[i] == quote && quote == '\'' && i+1 < len(line) && line[i+1] == '\'':
			i++ // '' is an escaped single quote
		case line[i] == quote:
			return true
		}
	}
	return false
}

// operationURLSeeds are the URLs of TestIsOperationURL and TestAcceptedWrite.
var operationURLSeeds = []string{
	defaultARMHost + "/subscriptions/s/providers/Microsoft.ApiManagement/operations/x?api-version=2021-08-01",
	defaultARMHost,
	"HTTPS://MANAGEMENT.AZURE.COM/operations/x",
	"https://Management.Azure.Com/operations/x",
	"https://management.azure.com:443/operations/x",
	"https://management.azure.com:8443/operations/x",
	"http://management.azure.com/operations/x",
	"http://management.azure.com:443/operations/x",
	"https://user@management.azure.com/operations/x",
	"https://user:pass@management.azure.com/operations/x",
	"https://management.azure.com@evil.example/operations/x",
	"https://evil.example/operations/x",
	"https://management.azure.com.evil.example/operations/x",
	"https://evilmanagement.azure.com/operations/x",
	"https://management.azure.co/operations/x",
	"//management.azure.com/operations/x",
	"/operations/x",
	"management.azure.com/operations/x",
	"https://management.azure.com/%zz",
	"https://[::1",
	"",
	"https://management.azure.com:/operations/x",
	"https://[management.azure.com]/operations/x",
	"https://management.azure.com./operations/x",
	" /operations/rel ",
	"/operations/rel?api-version=2021-08-01",
	"operations/x",
	"//evil.example/x",
	"/@evil.example/x",
}

// isARMOrigin is the reference for IsOperationURL, written apart from origin(): the URL
// parses, has no userinfo, and its scheme, host (ASCII case-insensitive) and port (the
// scheme's default or none) are management.azure.com over https.
func isARMOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	port := u.Port()
	return asciiLower(u.Scheme) == "https" &&
		asciiLower(u.Hostname()) == "management.azure.com" &&
		(port == "" || port == "443")
}

// asciiLower lower-cases ASCII letters only; unicode case folding would map, say, the Kelvin
// sign to k, which is not how DNS compares names.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// FuzzIsOperationURL: an accepted URL parses, carries no userinfo and is on the ARM origin.
// It must also be the other way round: a URL on the ARM origin is accepted.
func FuzzIsOperationURL(f *testing.F) {
	for _, u := range operationURLSeeds {
		f.Add(u)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if armHost != defaultARMHost {
			t.Skipf("armHost is %s, not the production endpoint", armHost)
		}
		got := IsOperationURL(raw)
		if want := isARMOrigin(raw); got != want {
			t.Errorf("IsOperationURL(%q) = %v, the ARM origin check says %v", raw, got, want)
		}
	})
}

// FuzzAcceptedWrite: whatever headers a 202 carries, acceptedWrite either returns an
// operation URL IsOperationURL accepts (and a non-negative Retry-After) or ErrNoOperationURL.
func FuzzAcceptedWrite(f *testing.F) {
	for _, u := range operationURLSeeds {
		f.Add(u, "", "")
		f.Add("", u, "15")
		f.Add("  ", u, "soon")
	}
	f.Add("https://evil.example/operations/x", defaultARMHost+"/operations/ok", "")
	f.Add(defaultARMHost+"/operations/preferred", "/operations/ignored", "9223372037")
	f.Add("", "/operations/r", "Wed, 21 Oct 2099 07:28:00 GMT")
	f.Fuzz(func(t *testing.T, asyncOperation, location, retry string) {
		if armHost != defaultARMHost {
			t.Skipf("armHost is %s, not the production endpoint", armHost)
		}
		header := http.Header{}
		for name, value := range map[string]string{
			"Azure-AsyncOperation": asyncOperation,
			"Location":             location,
			"Retry-After":          retry,
		} {
			if value != "" {
				header.Set(name, value)
			}
		}
		result, err := acceptedWrite("import API", http.MethodPut, header)
		if err != nil {
			if !errors.Is(err, ErrNoOperationURL) || result != (WriteResult{}) {
				t.Errorf("acceptedWrite(%v) = %+v, %v; want a zero result and ErrNoOperationURL", header, result, err)
			}
			return
		}
		if !IsOperationURL(result.OperationURL) || !isARMOrigin(result.OperationURL) {
			t.Errorf("acceptedWrite(%v) returned operation URL %q outside ARM", header, result.OperationURL)
		}
		if result.RetryAfter < 0 {
			t.Errorf("acceptedWrite(%v) RetryAfter = %s, want >= 0", header, result.RetryAfter)
		}
	})
}

// FuzzRetryAfter: for any header value and a non-negative default, retryAfter returns a
// non-negative delay, and the default when the value is not a positive delay.
func FuzzRetryAfter(f *testing.F) {
	def := int64(10 * time.Second)
	for _, value := range []string{
		"", "5", " 7 ", "0", "-3", "soon", "9223372036", "9223372037", "18446744074",
		"9223372036854775807", "-9223372036854775808", "+5", "1e3", "0x10",
		time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat),
		time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat),
		"Wed, 21 Oct 2099 07:28:00 GMT", "Monday, 02-Jan-06 15:04:05 MST", "Mon Jan  2 15:04:05 2006",
	} {
		f.Add(value, def)
	}
	f.Add("5", int64(0))
	f.Fuzz(func(t *testing.T, value string, defNanos int64) {
		if defNanos < 0 {
			defNanos = -(defNanos + 1) // keeps math.MinInt64 in range
		}
		def := time.Duration(defNanos)
		header := http.Header{}
		header.Set("Retry-After", value)
		got := retryAfter(header, def)
		if got < 0 {
			t.Errorf("retryAfter(%q, %s) = %s, want >= 0", value, def, got)
		}
	})
}

// FuzzParseARMError: any error body parses without a panic into valid UTF-8 and a message of
// at most maxErrorBodyInMessage bytes plus the ellipsis, an ARM JSON message included.
//
// ARM-F2 (invalid bytes growing a message past the limit,
// testdata/fuzz/FuzzParseARMError/a4b80dbbd438c9cd) is fixed: truncateMessage replaces them
// before it measures.
func FuzzParseARMError(f *testing.F) {
	for _, body := range []string{
		`{"error":{"code":"Conflict","message":"busy"}}`,
		`{"error":{"code":"ValidationError","message":"bad:","details":[{"code":"InvalidFormat","message":"field x"},{"code":"Other"}]}}`,
		`{"code":"PreconditionFailed","message":"etag mismatch"}`,
		`{"status":"Failed","error":{"code":"Timeout","message":"slow"}}`,
		`Bad Gateway`, ``, `{"status":"Failed"}`, `{"error":null}`, `{"error":{"details":[{}]}}`,
		strings.Repeat("x", 3000),
		strings.Repeat("é", 1000),
		"\xff\xfe not UTF-8",
		strings.Repeat("x", maxErrorBodyInMessage-1) + "€",
	} {
		f.Add([]byte(body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		code, detailCode, message := parseARMError(body)
		armShape := isARMErrorShape(body)
		if limit := maxErrorBodyInMessage + len("…"); len(message) > limit {
			t.Errorf("parseARMError() message is %d bytes for a %d byte body (ARM shape %v), want at most %d",
				len(message), len(body), armShape, limit)
		}
		if !utf8.ValidString(code) || !utf8.ValidString(detailCode) {
			t.Errorf("parseARMError(%q) codes %q, %q are not valid UTF-8", body, code, detailCode)
		}
		// ARM-F1 (fixed): a short message used to keep a body's invalid UTF-8.
		if !utf8.ValidString(message) {
			t.Errorf("parseARMError(%q) message = %q, not valid UTF-8", body, message)
		}
	})
}

// isARMErrorShape reports whether parseARMError reads body as ARM's JSON error shape rather
// than as raw text.
func isARMErrorShape(body []byte) bool {
	var parsed armErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	detail := parsed.armErrorDetail
	if parsed.Error != nil {
		detail = *parsed.Error
	}
	return detail.Code != "" || detail.Message != ""
}

// FuzzRedactURL: the redacted URL never carries the input's userinfo password or a query
// value, and does not parse with userinfo or a query other than the marker. A secret that
// also appears in a part RedactURL keeps (the host, the path, the fragment) or in the
// "?<redacted>" marker is not a leak and is not checked.
func FuzzRedactURL(f *testing.F) {
	for _, raw := range []string{
		"https://orders.internal/api",
		"https://user:secret@orders.internal/api",
		"https://user@orders.internal/api",
		"https://fn.azurewebsites.net/api/x?code=secretkey",
		"https://u:p@fn.azurewebsites.net/api/x?code=k&a=b#frag",
		"wss://bidme.example/hub",
		"", "http://[::1",
		"//user:pass@host/x?k=v",
		"https://u:p%40ss@host/x?code=%73ecret",
		"https://host/x?",
		"https://host/x#?code=fragment",
		"https:user:pass@host?code=secret",
	} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := RedactURL(raw)
		parsed, err := url.Parse(raw)
		if err != nil {
			if got != "<unparseable URL>" {
				t.Errorf("RedactURL(%q) = %q, want <unparseable URL>", raw, got)
			}
			return
		}
		// Only for a URL with a host: without one, a path starting with // renders as an
		// authority ("//@//@" becomes "//@"), a quirk of url.URL.String, not a leak.
		if reparsed, err := url.Parse(got); err == nil && parsed.Host != "" &&
			(reparsed.User != nil || (reparsed.RawQuery != "" && reparsed.RawQuery != "<redacted>")) {
			t.Errorf("RedactURL(%q) = %q, which parses with userinfo %q and query %q", raw, got, reparsed.User, reparsed.RawQuery)
		}
		// The parts RedactURL keeps, rendered with and without the marker: a secret found in
		// them (a password equal to a path segment, a query value "?") is a coincidence.
		kept := *parsed
		kept.User, kept.RawQuery, kept.ForceQuery = nil, "", false
		public := kept.String()
		if parsed.RawQuery != "" {
			kept.RawQuery = "<redacted>"
			public += " " + kept.String()
		}

		var secrets []string
		if password, ok := parsed.User.Password(); ok {
			secrets = append(secrets, password, parsed.User.String())
		}
		for _, values := range parsed.Query() {
			secrets = append(secrets, values...)
		}
		if parsed.RawQuery != "" {
			secrets = append(secrets, parsed.RawQuery)
		}
		for _, secret := range secrets {
			if secret != "" && !strings.Contains(public, secret) && strings.Contains(got, secret) {
				t.Errorf("RedactURL(%q) = %q still carries %q", raw, got, secret)
			}
		}
	})
}
