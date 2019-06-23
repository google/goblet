// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package goblet

import (
	"io"

	"github.com/google/gitprotocolio"
)

func writePacket(w io.Writer, p gitprotocolio.Packet) error {
	_, err := w.Write(p.EncodeToPktLine())
	return err
}

func writeResp(w io.Writer, chunks []*gitprotocolio.ProtocolV2ResponseChunk) error {
	for _, chunk := range chunks {
		if err := writePacket(w, chunk); err != nil {
			return err
		}
	}
	return nil
}

func writeError(w io.Writer, err error) error {
	return writePacket(w, gitprotocolio.ErrorPacket(err.Error()))
}

func copyRequestChunk(c *gitprotocolio.ProtocolV2RequestChunk) *gitprotocolio.ProtocolV2RequestChunk {
	r := *c
	if r.Argument != nil {
		b := make([]byte, len(r.Argument))
		copy(b, r.Argument)
		r.Argument = b
	}
	return &r
}

func copyResponseChunk(c *gitprotocolio.ProtocolV2ResponseChunk) *gitprotocolio.ProtocolV2ResponseChunk {
	r := *c
	if r.Response != nil {
		b := make([]byte, len(r.Response))
		copy(b, r.Response)
		r.Response = b
	}
	return &r
}
