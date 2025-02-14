package imphash

import (
	"bytes"
	"crypto/md5"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	
	"github.com/yalue/elf_reader"
	"github.com/glaslos/ssdeep"
)

type ImpHashResult struct {
	ImpHash   string
	ImpFuzzy  string
	ImpString string
}

func ImpHashFromBytes(fileContents []byte) (*ImpHashResult, error) {
	if bytes.HasPrefix(fileContents, []byte{0x4d, 0x5a}) {
		return impHashFromPEBytes(fileContents)
	}
	if bytes.HasPrefix(fileContents, []byte{0x7f, 0x45, 0x4c, 0x46}) {
		return impHashFromELFBytes(fileContents)
	}
	if bytes.HasPrefix(fileContents, []byte{0xfe, 0xed, 0xfa, 0xce}) || // 32-bit
		bytes.HasPrefix(fileContents, []byte{0xce, 0xfa, 0xed, 0xfe}) || // 32-bit, reverse ordering
		bytes.HasPrefix(fileContents, []byte{0xfe, 0xed, 0xfa, 0xcf}) || // 64-bit
		bytes.HasPrefix(fileContents, []byte{0xcf, 0xfa, 0xed, 0xfe}) { // 64-bit, reverse ordering
		return impHashFromMachO(fileContents)
	}
	if bytes.HasPrefix(fileContents, []byte{0xca, 0xfe, 0xba, 0xbe}) {
		return impHashFromFatMachO(fileContents)
	}
	return nil, errors.New("File type not supported")
}

var builderPool = sync.Pool{
	New: func() any {
		return new(strings.Builder)
	},
}

func impHashFromPEBytes(fileContents []byte) (*ImpHashResult, error) {
	fileReader := bytes.NewReader(fileContents)
	pefile, err := pe.NewFile(fileReader)
	if err != nil {
		return nil, err
	}

	defer pefile.Close()
	libs, err := pefile.ImportedSymbols()
	if err != nil {
		return nil, err
	}

	impHashes := &ImpHashResult{}

	dllNames := make([]string, 0)
	dllFunc := make(map[string][]string, 0)
	for _, lib := range libs {
		//fmt.Println(lib)
		if !strings.Contains(lib, ":") {
			continue
		}
		parts := strings.Split(lib, ":")
		dllName := strings.ToLower(parts[1])
		if strings.HasSuffix(dllName, ".dll") {
			dllName = strings.Replace(dllName, ".dll", "", 1)
		} else {
			if strings.HasSuffix(dllName, ".sys") {
				dllName = strings.Replace(dllName, ".sys", "", 1)
			}
		}
		funcName := strings.ToLower(parts[0])
		dllFunc[dllName] = append(dllFunc[dllName], funcName)
	}

	for dllName := range dllFunc {
		dllNames = append(dllNames, dllName)
	}

	sort.Strings(dllNames) // Gives a new ImpHash than Python's pefile, but now we don't care about reordering to evade ImpHash
	builder := builderPool.Get().(*strings.Builder)
	builder.Reset()
	for idx1, dllName := range dllNames {
		sort.Strings(dllFunc[dllName])
		for idx2, funcName := range dllFunc[dllName] {
			if idx1+idx2 > 0 {
				builder.WriteByte(',')
			}
			builder.Grow(len(dllName) + len(funcName) + 1)
			builder.WriteString(dllName)
			builder.WriteString(".")
			builder.WriteString(funcName)
		}
	}
	impHashes.ImpHash = fmt.Sprintf("%x", md5.Sum([]byte(builder.String())))
	for {
		if builder.Len() < 4096 {
			builder.WriteString(" ")
		} else {
			break
		}
	}
	impHashes.ImpString = builder.String()
	impHashes.ImpFuzzy, _ = ssdeep.FuzzyBytes([]byte(impHashes.ImpString))

	builderPool.Put(builder)

	return impHashes, nil
}

func sanityCheck(content []byte) error {
	e, err := elf_reader.ParseELFFile(content)
	if err != nil {
		return fmt.Errorf("parsing ELF: %w", err)
	}

	var sum uint64

	for i := uint16(0); i < e.GetSectionCount(); i++ {
		hdr, err := e.GetSectionHeader(i)
		if err != nil {
			return fmt.Errorf("getting header for section %d: %w", i, err)
		}

		if hdr.GetSize() > uint64(len(content)) {
			return fmt.Errorf("section %d too large: %d > %d", i, hdr.GetSize(), len(content))
		}

		sum += hdr.GetSize()
		if sum > uint64(len(content)) {
			return fmt.Errorf("sections up to %d too large: %d > %d", i, sum, len(content))
		}
	}

	return nil
}

func impHashFromELFBytes(fileContents []byte) (*ImpHashResult, error) {
	err := sanityCheck(fileContents)
	if err != nil {
		return nil, err
	}
		
	fileReader := bytes.NewReader(fileContents)
	e, err := elf.NewFile(fileReader)
	if err != nil {
		return nil, err
	}

	defer e.Close()
	
	libs, err := e.ImportedSymbols()
	if err != nil {
		return nil, err
	}

	libFunc := make(map[string][]string, 0)
	for _, lib := range libs {
		libname := lib.Library
		soIdx := strings.Index(libname, ".so")
		if soIdx > 0 {
			libname = libname[:soIdx]
		}
		libFunc[libname] = append(libFunc[libname], lib.Name)
	}

	libNames := make([]string, 0)
	for lib := range libFunc {
		libNames = append(libNames, lib)
	}
	sort.Strings(libNames)
	builder := strings.Builder{}
	for idx1, dllName := range libNames {
		sort.Strings(libFunc[dllName])
		for idx2, funcName := range libFunc[dllName] {
			if idx1+idx2 > 0 {
				builder.WriteByte(',')
			}
			builder.Grow(len(dllName) + len(funcName) + 1)
			builder.WriteString(dllName)
			builder.WriteString(".")
			builder.WriteString(funcName)
		}
	}

	impHashes := &ImpHashResult{}
	impHashes.ImpHash = fmt.Sprintf("%x", md5.Sum([]byte(builder.String())))

	for {
		if builder.Len() < 4096 {
			builder.WriteString(" ")
		} else {
			break
		}
	}
	impHashes.ImpString = builder.String()
	impHashes.ImpFuzzy, _ = ssdeep.FuzzyBytes([]byte(impHashes.ImpString))

	builderPool.Put(builder)

	return impHashes, nil
}

func impHashFromMachO(fileContents []byte) (*ImpHashResult, error) {
	fileReader := bytes.NewReader(fileContents)
	m, err := macho.NewFile(fileReader)
	if err != nil {
		return nil, err
	}

	libs, err := m.ImportedLibraries()
	if err != nil {
		return nil, err
	}

	libFunc := make(map[string]int, 0)
	for _, lib := range libs {
		libname := lib
		soIdx := strings.Index(libname, ".dylib")
		if soIdx > 0 {
			libname = libname[:soIdx]
		}
		libFunc[libname] = 1
	}

	symbols, err := m.ImportedSymbols()
	if err != nil {
		return nil, err
	}
	for _, symb := range symbols {
		libFunc[symb] = 1
	}

	libNames := make([]string, 0)
	for lib := range libFunc {
		libNames = append(libNames, lib)
	}
	sort.Strings(libNames)

	builder := builderPool.Get().(*strings.Builder)
	builder.Reset()
	for idx, dllName := range libNames {
		if idx > 0 {
			builder.WriteByte(',')
		}
		builder.Grow(len(dllName) + 1)
		builder.WriteString(dllName)
	}

	impHashes := &ImpHashResult{}
	impHashes.ImpHash = fmt.Sprintf("%x", md5.Sum([]byte(builder.String())))
	for {
		if builder.Len() < 4096 {
			builder.WriteString(" ")
		} else {
			break
		}
	}
	impHashes.ImpString = builder.String()
	impHashes.ImpFuzzy, _ = ssdeep.FuzzyBytes([]byte(impHashes.ImpString))

	builderPool.Put(builder)

	return impHashes, nil
}

func impHashFromFatMachO(fileContents []byte) (*ImpHashResult, error) {
	fileReader := bytes.NewReader(fileContents)
	m, err := macho.NewFatFile(fileReader)
	if err != nil {
		return nil, err
	}

	libFunc := make(map[string]int, 0) // Using it as a set
	for _, arch := range m.Arches {
		libs, err := arch.ImportedLibraries()
		if err != nil {
			return nil, err
		}

		for _, lib := range libs {
			libname := lib
			soIdx := strings.Index(libname, ".dylib")
			if soIdx > 0 {
				libname = libname[:soIdx]
			}
			libFunc[libname] = 1
		}

		symbols, err := arch.ImportedSymbols()
		if err != nil {
			return nil, err
		}
		for _, symb := range symbols {
			libFunc[symb] = 1
		}
	}

	libNames := make([]string, 0)
	for lib := range libFunc {
		libNames = append(libNames, lib)
	}

	sort.Strings(libNames)
	builder := builderPool.Get().(*strings.Builder)
	builder.Reset()
	for idx, dllName := range libNames {
		if idx > 0 {
			builder.WriteByte(',')
		}
		builder.Grow(len(dllName) + 1)
		builder.WriteString(dllName)
	}

	impHashes := &ImpHashResult{}
	impHashes.ImpHash = fmt.Sprintf("%x", md5.Sum([]byte(builder.String())))
	for {
		if builder.Len() < 4096 {
			builder.WriteString(" ")
		} else {
			break
		}
	}
	impHashes.ImpString = builder.String()
	impHashes.ImpFuzzy, _ = ssdeep.FuzzyBytes([]byte(impHashes.ImpString))

	builderPool.Put(builder)

	return impHashes, nil
}
