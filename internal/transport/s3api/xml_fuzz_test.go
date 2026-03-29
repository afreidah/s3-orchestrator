// -------------------------------------------------------------------------------
// XML Fuzz Tests - S3 Request Body Parsing
//
// Author: Alex Freidah
//
// Fuzz tests for XML parsing of S3 request bodies. Uses the streaming
// xml.NewDecoder path to match production (handleDeleteObjects and
// handleCompleteMultipartUpload use Decode, not Unmarshal).
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"encoding/xml"
	"testing"
)

func FuzzDeleteObjectsXML(f *testing.F) {
	f.Add([]byte(`<Delete><Quiet>false</Quiet><Object><Key>file.txt</Key></Object></Delete>`))
	f.Add([]byte(`<Delete><Object><Key>a</Key></Object><Object><Key>b</Key></Object></Delete>`))
	f.Add([]byte(`<Delete></Delete>`))
	f.Add([]byte(`<Delete><Quiet>true</Quiet></Delete>`))
	f.Add([]byte(`not xml at all`))
	f.Add([]byte(`<Delete><Object><Key></Key></Object></Delete>`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var req deleteObjectsRequest
		// Use streaming decoder to match production code path.
		// Empty keys and zero-length lists are valid XML parse results;
		// S3 spec validation is enforced at the handler layer.
		_ = xml.NewDecoder(bytes.NewReader(data)).Decode(&req)
	})
}

func FuzzCompleteMultipartXML(f *testing.F) {
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"abc"</ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>-1</PartNumber><ETag></ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>0</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>10001</PartNumber><ETag>x</ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`not xml`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var req completeMultipartUploadRequest
		// Use streaming decoder to match production code path.
		// Part number validation (1-10000) is enforced at the handler
		// layer, not at XML parse time.
		_ = xml.NewDecoder(bytes.NewReader(data)).Decode(&req)
	})
}
