// Copyright 2020 ETH Zurich
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package epic

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/binary"
	"math"
	"time"

	"github.com/scionproto/scion/pkg/addr"
	"github.com/scionproto/scion/pkg/private/serrors"
	"github.com/scionproto/scion/pkg/slayers"
	"github.com/scionproto/scion/pkg/slayers/path/epic"
)

const (
	// AuthLen denotes the size of the authenticator in bytes
	AuthLen = 16
	// MaxPacketLifetime denotes the maximal lifetime of a packet
	MaxPacketLifetime time.Duration = 2 * time.Second
	// MaxClockSkew denotes the maximal clock skew
	MaxClockSkew time.Duration = time.Second
	// TimestampResolution denotes the resolution of the epic timestamp
	TimestampResolution = 21 * time.Microsecond
	// MACBufferSize denotes the buffer size of the CBC input and output.
	MACBufferSize = 48
)

var zeroInitVector [16]byte

// CreateTimestamp returns the epic timestamp, which encodes the current time (now) relative to the
// input timestamp. The input timestamp must not be in the future (compared to the current time),
// otherwise an error is returned. An error is also returned if the current time is more than 1 day
// and 63 minutes after the input timestamp.
func CreateTimestamp(input time.Time, now time.Time) (uint32, error) {
	if input.After(now) {
		return 0, serrors.New("provided input timestamp is in the future",
			"input", input, "now", now)
	}
	epicTS := now.Sub(input)/TimestampResolution - 1
	if epicTS < 0 {
		epicTS = 0
	}
	if epicTS >= (1 << 32) {
		return 0, serrors.New("diff between input and now >1d63min", "epicTS", epicTS)
	}
	return uint32(epicTS), nil
}

// VerifyTimestamp checks whether an EPIC packet is fresh. This means that the time the packet
// was sent from the source host, which is encoded by the timestamp and the epicTimestamp,
// does not date back more than the maximal packet lifetime of two seconds. The function also takes
// a possible clock drift between the packet source and the verifier of up to one second into
// account.
func VerifyTimestamp(timestamp time.Time, epicTS uint32, now time.Time) error {
	diff := (time.Duration(epicTS) + 1) * TimestampResolution
	tsSender := timestamp.Add(diff)

	if tsSender.After(now.Add(MaxClockSkew)) {
		delta := tsSender.Sub(now.Add(MaxClockSkew))
		return serrors.New("epic timestamp is in the future",
			"delta", delta)
	}
	if now.After(tsSender.Add(MaxPacketLifetime).Add(MaxClockSkew)) {
		delta := now.Sub(tsSender.Add(MaxPacketLifetime).Add(MaxClockSkew))
		return serrors.New("epic timestamp expired",
			"delta", delta)
	}
	return nil
}

// CalcMac derives the EPIC MAC (PHVF/LHVF) given the full 16 bytes of the SCION path type
// MAC (auth), the EPIC packet ID (pktID), the timestamp in the Info Field (timestamp),
// and the SCION common/address header (s).
// If the same buffer is provided in subsequent calls to this function, the previously returned
// EPIC MAC may get overwritten. Only the most recently returned EPIC MAC is guaranteed to be
// valid.
func CalcMac(auth []byte, pktID epic.PktID, s *slayers.SCION,
	timestamp uint32, buffer []byte) ([]byte, error) {

	if len(buffer) < MACBufferSize {
		buffer = make([]byte, MACBufferSize)
	}

	// Initialize cryptographic MAC function
	f, err := initEpicMac(auth)
	if err != nil {
		return nil, err
	}
	// Prepare the input for the MAC function
	inputLength, err := prepareMacInput(pktID, s, timestamp, buffer)
	if err != nil {
		return nil, err
	}
	// Calculate Epic MAC = first 4 bytes of the last CBC block
	input := buffer[:inputLength]
	f.CryptBlocks(input, input)
	return input[len(input)-f.BlockSize() : len(input)-f.BlockSize()+4], nil
}

// VerifyHVF verifies the correctness of the HVF (PHVF or the LHVF) field in the EPIC packet by
// recalculating and comparing it. If the EPIC authenticator (auth), which denotes the full 16
// bytes of the SCION path type MAC, has invalid length, or if the MAC calculation gives an error,
// also VerifyHVF returns an error. The verification was successful if and only if VerifyHVF
// returns nil.
func VerifyHVF(auth []byte, pktID epic.PktID, s *slayers.SCION,
	timestamp uint32, hvf []byte, buffer []byte) error {

	if s == nil || len(auth) != AuthLen {
		return serrors.New("invalid input")
	}

	mac, err := CalcMac(auth, pktID, s, timestamp, buffer)
	if err != nil {
		return err
	}

	if subtle.ConstantTimeCompare(hvf, mac) == 0 {
		return serrors.New("epic hop validation field verification failed",
			"hvf in packet", hvf, "calculated mac", mac, "auth", auth)
	}
	return nil
}

// PktCounterFromCore creates a counter for the packet identifier
// based on the core ID and the core counter.
func PktCounterFromCore(coreID uint8, coreCounter uint32) uint32 {
	return (uint32(coreID) << 24) | (coreCounter & 0x00FFFFFF)
}

// CoreFromPktCounter reads the core ID and the core counter
// from a counter belonging to a packet identifier.
func CoreFromPktCounter(counter uint32) (uint8, uint32) {
	coreID := uint8(counter >> 24)
	coreCounter := counter & 0x00FFFFFF
	return coreID, coreCounter
}

func initEpicMac(key []byte) (cipher.BlockMode, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, serrors.New("Unable to initialize AES cipher")
	}

	// CBC-MAC = CBC-Encryption with zero initialization vector
	mode := cipher.NewCBCEncrypter(block, zeroInitVector[:])
	return mode, nil
}

func prepareMacInput(pktID epic.PktID, s *slayers.SCION, timestamp uint32,
	inputBuffer []byte) (int, error) {

	//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//   | flags (1B) | timestamp (4B) |    packet ID (8B)     |
	//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//   | srcIA (8B) | srcAddr (4/8/12/16B) | payloadLen (2B) |
	//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	//   | zero padding (0-15B)                                |
	//   +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	// The "flags" field only encodes the length of the source address.

	if s == nil {
		return 0, serrors.New("SCION common+address header must not be nil")
	}
	srcAddr := s.RawSrcAddr
	l := len(srcAddr)

	// Calculate a multiple of 16 such that the input fits in
	nrBlocks := int(math.Ceil((23 + float64(l)) / 16))
	inputLength := 16 * nrBlocks

	// Fill input
	offset := 0
	inputBuffer[0] = uint8(s.SrcAddrLen)
	offset += 1
	binary.BigEndian.PutUint32(inputBuffer[offset:], timestamp)
	offset += 4
	pktID.SerializeTo(inputBuffer[offset:])
	offset += epic.PktIDLen
	binary.BigEndian.PutUint64(inputBuffer[offset:], uint64(s.SrcIA))
	offset += addr.IABytes
	copy(inputBuffer[offset:], srcAddr)
	offset += l
	binary.BigEndian.PutUint16(inputBuffer[offset:], s.PayloadLen)
	offset += 2
	copy(inputBuffer[offset:inputLength], zeroInitVector[:])
	return inputLength, nil
}
