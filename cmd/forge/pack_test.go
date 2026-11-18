package main

import (
	"bytes"
	"crypto/sha1"
	"strings"
	"testing"
)

func testObjects() []*GitObject {
	blob := MakeObject(ObjBlob, []byte(strings.Repeat("payload line\n", 300)))
	empty := MakeObject(ObjBlob, []byte{})
	tree := MakeObject(ObjTree, BuildTree([]TreeEntry{{Mode: "100644", Name: "a.txt", OID: blob.OID}, {Mode: "100644", Name: "e", OID: empty.OID}}))
	return []*GitObject{tree, blob, empty}
}

func TestPackRoundTripKeepsStreams(t *testing.T) {
	objs := testObjects()
	pack := WritePack(objs)
	parsed, err := ParsePack(pack, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(objs) {
		t.Fatalf("%d objects, want %d", len(parsed), len(objs))
	}
	for i, obj := range parsed {
		if obj.OID != objs[i].OID || !bytes.Equal(obj.Data, objs[i].Data) {
			t.Fatalf("object %d: %s, want %s", i, obj.OID, objs[i].OID)
		}
		if !obj.IsDeflated() || !bytes.Equal(obj.Deflated(), objs[i].Deflated()) {
			t.Fatalf("object %d did not keep its received zlib stream", i)
		}
	}
	// Re-serving the parsed objects reuses those streams byte for byte.
	if again := WritePack(parsed); !bytes.Equal(again, pack) {
		t.Fatal("re-written pack differs")
	}
}

func resealPack(pack []byte) []byte {
	sum := sha1.Sum(pack[:len(pack)-20])
	copy(pack[len(pack)-20:], sum[:])
	return pack
}

func TestPackRejectsCorruptZlib(t *testing.T) {
	objs := testObjects()
	pack := WritePack(objs)
	// The first object's stream starts after the 12-byte pack header and its entry header.
	start := 12 + entryHeaderLen(len(objs[0].Data))
	stream := objs[0].Deflated()

	badAdler := resealPack(bytes.Clone(pack))
	badAdler[start+len(stream)-1] ^= 0xff
	resealPack(badAdler)
	if _, err := ParsePack(badAdler, nil); err == nil || !strings.Contains(err.Error(), "invalid checksum") {
		t.Fatalf("corrupt Adler-32: %v", err)
	}

	badHeader := bytes.Clone(pack)
	badHeader[start] = 0x79 // CM = 9: not deflate
	resealPack(badHeader)
	if _, err := ParsePack(badHeader, nil); err == nil || !strings.Contains(err.Error(), "invalid header") {
		t.Fatalf("corrupt zlib header: %v", err)
	}
}
