package server

import (
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
		// Must not panic regardless of input.
		_ = xml.Unmarshal(data, &req)
	})
}

func FuzzCompleteMultipartXML(f *testing.F) {
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"abc"</ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>-1</PartNumber><ETag></ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`not xml`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var req completeMultipartUploadRequest
		// Must not panic regardless of input.
		_ = xml.Unmarshal(data, &req)
	})
}
