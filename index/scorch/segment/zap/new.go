//  Copyright (c) 2018 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring"
	"github.com/Smerity/govarint"
	"github.com/blevesearch/bleve/analysis"
	"github.com/blevesearch/bleve/document"
	"github.com/blevesearch/bleve/index"
	"github.com/couchbase/vellum"
	"github.com/golang/snappy"
)

// AnalysisResultsToSegmentBase produces an in-memory zap-encoded
// SegmentBase from analysis results
func AnalysisResultsToSegmentBase(results []*index.AnalysisResult,
	chunkFactor uint32) (*SegmentBase, error) {
	s := interimPool.Get().(*interim)

	var br bytes.Buffer
	if s.lastNumDocs > 0 {
		// use previous results to initialize the buf with an estimate
		// size, but note that the interim instance comes from a
		// global interimPool, so multiple scorch instances indexing
		// different docs can lead to low quality estimates
		avgBytesPerDoc := s.lastOutSize / s.lastNumDocs
		br.Grow(avgBytesPerDoc * (len(results) + 1))
	}

	s.results = results
	s.chunkFactor = chunkFactor
	s.w = NewCountHashWriter(&br)

	storedIndexOffset, fieldsIndexOffset, fdvIndexOffset, dictOffsets,
		err := s.convert()
	if err != nil {
		return nil, err
	}

	sb, err := InitSegmentBase(br.Bytes(), s.w.Sum32(), chunkFactor,
		s.FieldsMap, s.FieldsInv, uint64(len(results)),
		storedIndexOffset, fieldsIndexOffset, fdvIndexOffset, dictOffsets)

	if err == nil && s.reset() == nil {
		s.lastNumDocs = len(results)
		s.lastOutSize = len(br.Bytes())
		interimPool.Put(s)
	}

	return sb, err
}

var interimPool = sync.Pool{New: func() interface{} { return &interim{} }}

// interim holds temporary working data used while converting from
// analysis results to a zap-encoded segment
type interim struct {
	results []*index.AnalysisResult

	chunkFactor uint32

	w *CountHashWriter

	// FieldsMap adds 1 to field id to avoid zero value issues
	//  name -> field id + 1
	FieldsMap map[string]uint16

	// FieldsInv is the inverse of FieldsMap
	//  field id -> name
	FieldsInv []string

	// Term dictionaries for each field
	//  field id -> term -> postings list id + 1
	Dicts []map[string]uint64

	// Terms for each field, where terms are sorted ascending
	//  field id -> []term
	DictKeys [][]string

	// Fields whose IncludeDocValues is true
	//  field id -> bool
	IncludeDocValues []bool

	// postings id -> bitmap of docNums
	Postings []*roaring.Bitmap

	// postings id -> bitmap of docNums that have locations
	PostingsLocs []*roaring.Bitmap

	// postings id -> freq/norm's, one for each docNum in postings
	FreqNorms        [][]interimFreqNorm
	freqNormsBacking []interimFreqNorm

	// postings id -> locs, one for each freq
	Locs        [][]interimLoc
	locsBacking []interimLoc

	numTermsPerPostingsList []int // key is postings list id
	numLocsPerPostingsList  []int // key is postings list id

	builder    *vellum.Builder
	builderBuf bytes.Buffer

	metaBuf bytes.Buffer

	tmp0 []byte
	tmp1 []byte

	lastNumDocs int
	lastOutSize int
}

func (s *interim) reset() (err error) {
	s.results = nil
	s.chunkFactor = 0
	s.w = nil
	s.FieldsMap = nil
	s.FieldsInv = nil
	for i := range s.Dicts {
		s.Dicts[i] = nil
	}
	s.Dicts = s.Dicts[:0]
	for i := range s.DictKeys {
		s.DictKeys[i] = s.DictKeys[i][:0]
	}
	s.DictKeys = s.DictKeys[:0]
	for i := range s.IncludeDocValues {
		s.IncludeDocValues[i] = false
	}
	s.IncludeDocValues = s.IncludeDocValues[:0]
	for _, idn := range s.Postings {
		idn.Clear()
	}
	s.Postings = s.Postings[:0]
	for _, idn := range s.PostingsLocs {
		idn.Clear()
	}
	s.PostingsLocs = s.PostingsLocs[:0]
	s.FreqNorms = s.FreqNorms[:0]
	for i := range s.freqNormsBacking {
		s.freqNormsBacking[i] = interimFreqNorm{}
	}
	s.freqNormsBacking = s.freqNormsBacking[:0]
	s.Locs = s.Locs[:0]
	for i := range s.locsBacking {
		s.locsBacking[i] = interimLoc{}
	}
	s.locsBacking = s.locsBacking[:0]
	s.numTermsPerPostingsList = s.numTermsPerPostingsList[:0]
	s.numLocsPerPostingsList = s.numLocsPerPostingsList[:0]
	s.builderBuf.Reset()
	if s.builder != nil {
		err = s.builder.Reset(&s.builderBuf)
	}
	s.metaBuf.Reset()
	s.tmp0 = s.tmp0[:0]
	s.tmp1 = s.tmp1[:0]
	s.lastNumDocs = 0
	s.lastOutSize = 0

	return err
}

func (s *interim) grabBuf(size int) []byte {
	buf := s.tmp0
	if cap(buf) < size {
		buf = make([]byte, size)
		s.tmp0 = buf
	}
	return buf[0:size]
}

type interimStoredField struct {
	vals      [][]byte
	typs      []byte
	arrayposs [][]uint64 // array positions
}

type interimFreqNorm struct {
	freq uint64
	norm float32
}

type interimLoc struct {
	fieldID   uint16
	pos       uint64
	start     uint64
	end       uint64
	arrayposs []uint64
}

func (s *interim) convert() (uint64, uint64, uint64, []uint64, error) {
	s.FieldsMap = map[string]uint16{}

	s.getOrDefineField("_id") // _id field is fieldID 0

	for _, result := range s.results {
		for _, field := range result.Document.CompositeFields {
			s.getOrDefineField(field.Name())
		}
		for _, field := range result.Document.Fields {
			s.getOrDefineField(field.Name())
		}
	}

	sort.Strings(s.FieldsInv[1:]) // keep _id as first field

	for fieldID, fieldName := range s.FieldsInv {
		s.FieldsMap[fieldName] = uint16(fieldID + 1)
	}

	if cap(s.IncludeDocValues) >= len(s.FieldsInv) {
		s.IncludeDocValues = s.IncludeDocValues[:len(s.FieldsInv)]
	} else {
		s.IncludeDocValues = make([]bool, len(s.FieldsInv))
	}

	s.prepareDicts()

	for _, dict := range s.DictKeys {
		sort.Strings(dict)
	}

	s.processDocuments()

	storedIndexOffset, err := s.writeStoredFields()
	if err != nil {
		return 0, 0, 0, nil, err
	}

	var fdvIndexOffset uint64
	var dictOffsets []uint64

	if len(s.results) > 0 {
		fdvIndexOffset, dictOffsets, err = s.writeDicts()
		if err != nil {
			return 0, 0, 0, nil, err
		}
	} else {
		dictOffsets = make([]uint64, len(s.FieldsInv))
	}

	fieldsIndexOffset, err := persistFields(s.FieldsInv, s.w, dictOffsets)
	if err != nil {
		return 0, 0, 0, nil, err
	}

	return storedIndexOffset, fieldsIndexOffset, fdvIndexOffset, dictOffsets, nil
}

func (s *interim) getOrDefineField(fieldName string) int {
	fieldIDPlus1, exists := s.FieldsMap[fieldName]
	if !exists {
		fieldIDPlus1 = uint16(len(s.FieldsInv) + 1)
		s.FieldsMap[fieldName] = fieldIDPlus1
		s.FieldsInv = append(s.FieldsInv, fieldName)

		s.Dicts = append(s.Dicts, make(map[string]uint64))

		n := len(s.DictKeys)
		if n < cap(s.DictKeys) {
			s.DictKeys = s.DictKeys[:n+1]
			s.DictKeys[n] = s.DictKeys[n][:0]
		} else {
			s.DictKeys = append(s.DictKeys, []string(nil))
		}
	}

	return int(fieldIDPlus1 - 1)
}

// the fieldName must be for a known field
func (s *interim) getField(fieldName string) int {
	fieldIDPlus1, exists := s.FieldsMap[fieldName]
	if !exists || fieldIDPlus1 <= 0 {
		panic(fmt.Sprintf("getField saw unknown fieldName: %s, fieldsMap: %#v",
			fieldName, s.FieldsMap))
	}
	return int(fieldIDPlus1 - 1)
}

// fill Dicts and DictKeys from analysis results
func (s *interim) prepareDicts() {
	var pidNext int

	var totTFs int
	var totLocs int

	visitField := func(fieldID uint16, tfs analysis.TokenFrequencies) {
		dict := s.Dicts[fieldID]
		dictKeys := s.DictKeys[fieldID]

		for term, tf := range tfs {
			pidPlus1, exists := dict[term]
			if !exists {
				pidNext++
				pidPlus1 = uint64(pidNext)

				dict[term] = pidPlus1
				dictKeys = append(dictKeys, term)

				s.numTermsPerPostingsList = append(s.numTermsPerPostingsList, 0)
				s.numLocsPerPostingsList = append(s.numLocsPerPostingsList, 0)
			}

			pid := pidPlus1 - 1

			s.numTermsPerPostingsList[pid] += 1
			s.numLocsPerPostingsList[pid] += len(tf.Locations)

			totLocs += len(tf.Locations)
		}

		totTFs += len(tfs)

		s.DictKeys[fieldID] = dictKeys
	}

	for _, result := range s.results {
		// walk each composite field
		for _, field := range result.Document.CompositeFields {
			fieldID := uint16(s.getField(field.Name()))
			_, tf := field.Analyze()
			visitField(fieldID, tf)
		}

		// walk each field
		for i, field := range result.Document.Fields {
			fieldID := uint16(s.getField(field.Name()))
			tf := result.Analyzed[i]
			visitField(fieldID, tf)
		}
	}

	numPostingsLists := pidNext

	if cap(s.Postings) >= numPostingsLists {
		s.Postings = s.Postings[:numPostingsLists]
	} else {
		postings := make([]*roaring.Bitmap, numPostingsLists)
		copy(postings, s.Postings[:cap(s.Postings)])
		for i := 0; i < numPostingsLists; i++ {
			if postings[i] == nil {
				postings[i] = roaring.New()
			}
		}
		s.Postings = postings
	}

	if cap(s.PostingsLocs) >= numPostingsLists {
		s.PostingsLocs = s.PostingsLocs[:numPostingsLists]
	} else {
		postingsLocs := make([]*roaring.Bitmap, numPostingsLists)
		copy(postingsLocs, s.PostingsLocs[:cap(s.PostingsLocs)])
		for i := 0; i < numPostingsLists; i++ {
			if postingsLocs[i] == nil {
				postingsLocs[i] = roaring.New()
			}
		}
		s.PostingsLocs = postingsLocs
	}

	if cap(s.FreqNorms) >= numPostingsLists {
		s.FreqNorms = s.FreqNorms[:numPostingsLists]
	} else {
		s.FreqNorms = make([][]interimFreqNorm, numPostingsLists)
	}

	if cap(s.freqNormsBacking) >= totTFs {
		s.freqNormsBacking = s.freqNormsBacking[:totTFs]
	} else {
		s.freqNormsBacking = make([]interimFreqNorm, totTFs)
	}

	freqNormsBacking := s.freqNormsBacking
	for pid, numTerms := range s.numTermsPerPostingsList {
		s.FreqNorms[pid] = freqNormsBacking[0:0]
		freqNormsBacking = freqNormsBacking[numTerms:]
	}

	if cap(s.Locs) >= numPostingsLists {
		s.Locs = s.Locs[:numPostingsLists]
	} else {
		s.Locs = make([][]interimLoc, numPostingsLists)
	}

	if cap(s.locsBacking) >= totLocs {
		s.locsBacking = s.locsBacking[:totLocs]
	} else {
		s.locsBacking = make([]interimLoc, totLocs)
	}

	locsBacking := s.locsBacking
	for pid, numLocs := range s.numLocsPerPostingsList {
		s.Locs[pid] = locsBacking[0:0]
		locsBacking = locsBacking[numLocs:]
	}
}

func (s *interim) processDocuments() {
	numFields := len(s.FieldsInv)
	reuseFieldLens := make([]int, numFields)
	reuseFieldTFs := make([]analysis.TokenFrequencies, numFields)

	for docNum, result := range s.results {
		for i := 0; i < numFields; i++ { // clear these for reuse
			reuseFieldLens[i] = 0
			reuseFieldTFs[i] = nil
		}

		s.processDocument(uint64(docNum), result,
			reuseFieldLens, reuseFieldTFs)
	}
}

func (s *interim) processDocument(docNum uint64,
	result *index.AnalysisResult,
	fieldLens []int, fieldTFs []analysis.TokenFrequencies) {
	visitField := func(fieldID uint16, fieldName string,
		ln int, tf analysis.TokenFrequencies) {
		fieldLens[fieldID] += ln

		existingFreqs := fieldTFs[fieldID]
		if existingFreqs != nil {
			existingFreqs.MergeAll(fieldName, tf)
		} else {
			fieldTFs[fieldID] = tf
		}
	}

	// walk each composite field
	for _, field := range result.Document.CompositeFields {
		fieldID := uint16(s.getField(field.Name()))
		ln, tf := field.Analyze()
		visitField(fieldID, field.Name(), ln, tf)
	}

	// walk each field
	for i, field := range result.Document.Fields {
		fieldID := uint16(s.getField(field.Name()))
		ln := result.Length[i]
		tf := result.Analyzed[i]
		visitField(fieldID, field.Name(), ln, tf)
	}

	// now that it's been rolled up into fieldTFs, walk that
	for fieldID, tfs := range fieldTFs {
		dict := s.Dicts[fieldID]
		norm := float32(1.0 / math.Sqrt(float64(fieldLens[fieldID])))

		for term, tf := range tfs {
			pid := dict[term] - 1
			bs := s.Postings[pid]
			bs.Add(uint32(docNum))

			s.FreqNorms[pid] = append(s.FreqNorms[pid],
				interimFreqNorm{
					freq: uint64(tf.Frequency()),
					norm: norm,
				})

			if len(tf.Locations) > 0 {
				locBS := s.PostingsLocs[pid]
				locBS.Add(uint32(docNum))

				locs := s.Locs[pid]

				for _, loc := range tf.Locations {
					var locf = uint16(fieldID)
					if loc.Field != "" {
						locf = uint16(s.getField(loc.Field))
					}
					var arrayposs []uint64
					if len(loc.ArrayPositions) > 0 {
						arrayposs = loc.ArrayPositions
					}
					locs = append(locs, interimLoc{
						fieldID:   locf,
						pos:       uint64(loc.Position),
						start:     uint64(loc.Start),
						end:       uint64(loc.End),
						arrayposs: arrayposs,
					})
				}

				s.Locs[pid] = locs
			}
		}
	}
}

func (s *interim) writeStoredFields() (
	storedIndexOffset uint64, err error) {
	metaEncoder := govarint.NewU64Base128Encoder(&s.metaBuf)

	data, compressed := s.tmp0[:0], s.tmp1[:0]
	defer func() { s.tmp0, s.tmp1 = data, compressed }()

	// keyed by docNum
	docStoredOffsets := make([]uint64, len(s.results))

	// keyed by fieldID, for the current doc in the loop
	docStoredFields := map[uint16]interimStoredField{}

	for docNum, result := range s.results {
		for fieldID := range docStoredFields { // reset for next doc
			delete(docStoredFields, fieldID)
		}

		for _, field := range result.Document.Fields {
			fieldID := uint16(s.getField(field.Name()))

			opts := field.Options()

			if opts.IsStored() {
				isf := docStoredFields[fieldID]
				isf.vals = append(isf.vals, field.Value())
				isf.typs = append(isf.typs, encodeFieldType(field))
				isf.arrayposs = append(isf.arrayposs, field.ArrayPositions())
				docStoredFields[fieldID] = isf
			}

			if opts.IncludeDocValues() {
				s.IncludeDocValues[fieldID] = true
			}
		}

		var curr int

		s.metaBuf.Reset()
		data = data[:0]
		compressed = compressed[:0]

		for fieldID := range s.FieldsInv {
			isf, exists := docStoredFields[uint16(fieldID)]
			if exists {
				curr, data, err = persistStoredFieldValues(
					fieldID, isf.vals, isf.typs, isf.arrayposs,
					curr, metaEncoder, data)
				if err != nil {
					return 0, err
				}
			}
		}

		metaEncoder.Close()
		metaBytes := s.metaBuf.Bytes()

		compressed = snappy.Encode(compressed, data)

		docStoredOffsets[docNum] = uint64(s.w.Count())

		_, err := writeUvarints(s.w,
			uint64(len(metaBytes)),
			uint64(len(compressed)))
		if err != nil {
			return 0, err
		}

		_, err = s.w.Write(metaBytes)
		if err != nil {
			return 0, err
		}

		_, err = s.w.Write(compressed)
		if err != nil {
			return 0, err
		}
	}

	storedIndexOffset = uint64(s.w.Count())

	for _, docStoredOffset := range docStoredOffsets {
		err = binary.Write(s.w, binary.BigEndian, docStoredOffset)
		if err != nil {
			return 0, err
		}
	}

	return storedIndexOffset, nil
}

func (s *interim) writeDicts() (fdvIndexOffset uint64, dictOffsets []uint64, err error) {
	dictOffsets = make([]uint64, len(s.FieldsInv))

	fdvOffsets := make([]uint64, len(s.FieldsInv))

	buf := s.grabBuf(binary.MaxVarintLen64)

	tfEncoder := newChunkedIntCoder(uint64(s.chunkFactor), uint64(len(s.results)-1))
	locEncoder := newChunkedIntCoder(uint64(s.chunkFactor), uint64(len(s.results)-1))
	fdvEncoder := newChunkedContentCoder(uint64(s.chunkFactor), uint64(len(s.results)-1))

	var docTermMap [][]byte

	if s.builder == nil {
		s.builder, err = vellum.New(&s.builderBuf, nil)
		if err != nil {
			return 0, nil, err
		}
	}

	for fieldID, terms := range s.DictKeys {
		if cap(docTermMap) < len(s.results) {
			docTermMap = make([][]byte, len(s.results))
		} else {
			docTermMap = docTermMap[0:len(s.results)]
			for docNum := range docTermMap { // reset the docTermMap
				docTermMap[docNum] = docTermMap[docNum][:0]
			}
		}

		dict := s.Dicts[fieldID]

		for _, term := range terms { // terms are already sorted
			pid := dict[term] - 1

			postingsBS := s.Postings[pid]
			postingsLocsBS := s.PostingsLocs[pid]

			freqNorms := s.FreqNorms[pid]
			freqNormOffset := 0

			locs := s.Locs[pid]
			locOffset := 0

			postingsItr := postingsBS.Iterator()
			for postingsItr.HasNext() {
				docNum := uint64(postingsItr.Next())

				freqNorm := freqNorms[freqNormOffset]

				err = tfEncoder.Add(docNum, freqNorm.freq,
					uint64(math.Float32bits(freqNorm.norm)))
				if err != nil {
					return 0, nil, err
				}

				for i := uint64(0); i < freqNorm.freq; i++ {
					if len(locs) > 0 {
						loc := locs[locOffset]

						err = locEncoder.Add(docNum, uint64(loc.fieldID),
							loc.pos, loc.start, loc.end,
							uint64(len(loc.arrayposs)))
						if err != nil {
							return 0, nil, err
						}

						err = locEncoder.Add(docNum, loc.arrayposs...)
						if err != nil {
							return 0, nil, err
						}
					}

					locOffset++
				}

				freqNormOffset++

				docTermMap[docNum] = append(
					append(docTermMap[docNum], term...),
					termSeparator)
			}

			tfEncoder.Close()
			locEncoder.Close()

			postingsOffset, err := writePostings(
				postingsBS, postingsLocsBS, tfEncoder, locEncoder,
				nil, s.w, buf)
			if err != nil {
				return 0, nil, err
			}

			if postingsOffset > uint64(0) {
				err = s.builder.Insert([]byte(term), postingsOffset)
				if err != nil {
					return 0, nil, err
				}
			}

			tfEncoder.Reset()
			locEncoder.Reset()
		}

		err = s.builder.Close()
		if err != nil {
			return 0, nil, err
		}

		// record where this dictionary starts
		dictOffsets[fieldID] = uint64(s.w.Count())

		vellumData := s.builderBuf.Bytes()

		// write out the length of the vellum data
		n := binary.PutUvarint(buf, uint64(len(vellumData)))
		_, err = s.w.Write(buf[:n])
		if err != nil {
			return 0, nil, err
		}

		// write this vellum to disk
		_, err = s.w.Write(vellumData)
		if err != nil {
			return 0, nil, err
		}

		// reset vellum for reuse
		s.builderBuf.Reset()

		err = s.builder.Reset(&s.builderBuf)
		if err != nil {
			return 0, nil, err
		}

		// write the field doc values
		if s.IncludeDocValues[fieldID] {
			for docNum, docTerms := range docTermMap {
				if len(docTerms) > 0 {
					err = fdvEncoder.Add(uint64(docNum), docTerms)
					if err != nil {
						return 0, nil, err
					}
				}
			}
			err = fdvEncoder.Close()
			if err != nil {
				return 0, nil, err
			}

			fdvOffsets[fieldID] = uint64(s.w.Count())

			_, err = fdvEncoder.Write(s.w)
			if err != nil {
				return 0, nil, err
			}

			fdvEncoder.Reset()
		} else {
			fdvOffsets[fieldID] = fieldNotUninverted
		}
	}

	fdvIndexOffset = uint64(s.w.Count())

	for _, fdvOffset := range fdvOffsets {
		n := binary.PutUvarint(buf, fdvOffset)
		_, err := s.w.Write(buf[:n])
		if err != nil {
			return 0, nil, err
		}
	}

	return fdvIndexOffset, dictOffsets, nil
}

func encodeFieldType(f document.Field) byte {
	fieldType := byte('x')
	switch f.(type) {
	case *document.TextField:
		fieldType = 't'
	case *document.NumericField:
		fieldType = 'n'
	case *document.DateTimeField:
		fieldType = 'd'
	case *document.BooleanField:
		fieldType = 'b'
	case *document.GeoPointField:
		fieldType = 'g'
	case *document.CompositeField:
		fieldType = 'c'
	}
	return fieldType
}
