// Package winres 生成 Windows 可执行文件需要的资源目标文件（.syso）。
//
// 用 Go 直接编译出来的 Windows 程序没有任何元数据：属性页里看不到产品名和版本，
// 也没有声明自己适配哪些 Windows 版本、要不要管理员权限。把这里生成的 .syso
// 放进程序所在目录（文件名带 _windows_amd64 这类后缀），go build 就会把它链进 exe。
//
// 目前只写两样资源：版本信息（RT_VERSION，决定属性页里显示什么）和使用清单
// （RT_MANIFEST，决定以什么权限运行、声明支持哪些 Windows 版本）。不写图标。
// 生成过程不依赖任何第三方工具，也不依赖 Windows 环境。
//
// 用法：在构建脚本里生成文件，再正常 go build：
//
//	data, err := winres.Object(winres.ArchAMD64, winres.Options{
//		FileVersion:     "1.2.3",
//		ProductName:     "示例程序",
//		FileDescription: "示例程序主窗口",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	// 文件名必须带目标平台后缀，Go 只会在对应平台构建时把它交给链接器。
//	if err := os.WriteFile("cmd/app/rsrc_windows_amd64.syso", data, 0o644); err != nil {
//		log.Fatal(err)
//	}
//
// 版本信息里的中文按 UTF-16 写入；同样的 Options 每次生成的字节完全一致，
// 便于比较两次构建的产物。写了资源并不会让程序变成「已签名」：Windows 的
// SmartScreen 仍然按代码签名证书判断发布者，这里只是让属性页和兼容性声明正常。
package winres

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Arch 是目标程序的 CPU 架构，决定 COFF 目标文件里的机器类型和重定位类型。
type Arch string

const (
	Arch386   Arch = "386"
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
)

// Options 是写进版本信息里的内容。除 FileVersion 外都可以留空，
// 留空的字段不会出现在属性页里。
type Options struct {
	// FileVersion 是程序自己的版本号，形如 1.2.3 或 1.2.3.4。
	// 为了方便比较，不足四段的补 0。
	FileVersion string
	// ProductVersion 是产品版本号，留空时沿用 FileVersion。
	ProductVersion   string
	ProductName      string
	FileDescription  string
	CompanyName      string
	Copyright        string
	InternalName     string
	OriginalFilename string

	// Manifest 是使用清单的 XML 文本。留空时用 DefaultManifest 生成一份。
	Manifest string
	// AssemblyName 是清单里的程序标识，只能用英文字母、数字、点和横线。
	// 留空时用 Application。
	AssemblyName string
	// AssemblyVersion 是清单里的程序版本，必须是四段数字。留空时取 FileVersion。
	AssemblyVersion string
	// Language 是资源语言，默认 0x0409（英语·美国）。
	Language uint16
	// Codepage 是版本信息的代码页，默认 1200（UTF-16）。
	Codepage uint16
}

// DefaultManifest 是一份保守的使用清单：以调用者身份运行（不申请管理员权限），
// 声明支持 Windows 7 到 Windows 11，并开启长路径和按显示器缩放。
func DefaultManifest(name, version string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="%s" version="%s" processorArchitecture="*"/>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{e2011457-1546-43c5-a5fe-008deee3d3f0}"/>
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">permonitorv2,permonitor</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
`, name, version)
}

// 资源类型编号（Win32 约定值）。
const (
	rtVersion  = 16
	rtManifest = 24
)

// Object 生成可以直接放进包目录的 .syso 内容。
// 同样的输入每次生成的字节完全一致，方便比较两次打包的产物。
func Object(arch Arch, opt Options) ([]byte, error) {
	if _, _, ok := machineFor(arch); !ok {
		return nil, fmt.Errorf("不支持的目标架构 %q", arch)
	}
	if opt.FileVersion == "" {
		opt.FileVersion = "1.0.0"
	}
	if opt.ProductVersion == "" {
		opt.ProductVersion = opt.FileVersion
	}
	if opt.Language == 0 {
		opt.Language = 0x0409
	}
	if opt.Codepage == 0 {
		opt.Codepage = 1200
	}
	assemblyName := opt.AssemblyName
	if assemblyName == "" {
		assemblyName = "Application"
	}
	assemblyVersion := opt.AssemblyVersion
	if assemblyVersion == "" {
		assemblyVersion = fourPartVersion(opt.FileVersion)
	}
	manifest := opt.Manifest
	if strings.TrimSpace(manifest) == "" {
		manifest = DefaultManifest(assemblyName, assemblyVersion)
	}

	items := []resource{
		{
			typ:      rtVersion,
			name:     1,
			lang:     uint32(opt.Language),
			data:     versionInfo(opt),
			codepage: uint32(opt.Codepage),
		},
		{
			typ:      rtManifest,
			name:     1,
			lang:     uint32(opt.Language),
			data:     []byte(manifest),
			codepage: 0,
		},
	}
	dir, payload, relocs := buildResourceTree(items)
	return coffObject(arch, dir, payload, relocs), nil
}

// ---------- 资源目录 ----------

type resource struct {
	typ      uint32
	name     uint32
	lang     uint32
	codepage uint32
	data     []byte
}

type resourceDir struct {
	entries []*resourceEntry
	offset  uint32
}

type resourceEntry struct {
	id   uint32
	dir  *resourceDir
	data *resourceData
}

type resourceData struct {
	payload     []byte
	codepage    uint32
	entryOffset uint32 // IMAGE_RESOURCE_DATA_ENTRY 在资源目录里的偏移
	payloadOff  uint32 // 内容在数据区里的偏移，重定位时填进 OffsetToData
}

func (dir *resourceDir) sub(id uint32) *resourceDir {
	for _, entry := range dir.entries {
		if entry.id == id && entry.dir != nil {
			return entry.dir
		}
	}
	child := &resourceDir{}
	dir.entries = append(dir.entries, &resourceEntry{id: id, dir: child})
	return child
}

// sortEntries 让同样的输入一定排出同样的字节，资源目录按编号升序。
func (dir *resourceDir) sortEntries() {
	sort.SliceStable(dir.entries, func(i, j int) bool { return dir.entries[i].id < dir.entries[j].id })
	for _, entry := range dir.entries {
		if entry.dir != nil {
			entry.dir.sortEntries()
		}
	}
}

// walk 按先根顺序列出所有目录，并顺手收集数据项。
func (dir *resourceDir) walk(dirs *[]*resourceDir, leaves *[]*resourceData) {
	*dirs = append(*dirs, dir)
	for _, entry := range dir.entries {
		if entry.dir != nil {
			entry.dir.walk(dirs, leaves)
			continue
		}
		if entry.data != nil {
			*leaves = append(*leaves, entry.data)
		}
	}
}

// buildResourceTree 排出一份 .rsrc 目录和它指向的数据区。
// 返回的 relocs 是 OffsetToData 字段在目录里的偏移，链接器会把它们
// 改写成数据在最终 exe 里的地址。
func buildResourceTree(items []resource) (dir []byte, payload []byte, relocs []uint32) {
	root := &resourceDir{}
	for _, item := range items {
		typeDir := root.sub(item.typ)
		nameDir := typeDir.sub(item.name)
		nameDir.entries = append(nameDir.entries, &resourceEntry{
			id:   item.lang,
			data: &resourceData{payload: item.data, codepage: item.codepage},
		})
	}
	root.sortEntries()

	var dirs []*resourceDir
	var leaves []*resourceData
	root.walk(&dirs, &leaves)

	var dirEnd uint32
	for _, d := range dirs {
		d.offset = dirEnd
		dirEnd += uint32(16 + 8*len(d.entries))
	}
	for _, leaf := range leaves {
		leaf.entryOffset = dirEnd
		dirEnd += 16
	}

	dir = make([]byte, dirEnd)

	var dataBuf bytes.Buffer
	for _, leaf := range leaves {
		for dataBuf.Len()%8 != 0 {
			dataBuf.WriteByte(0)
		}
		leaf.payloadOff = uint32(dataBuf.Len())
		dataBuf.Write(leaf.payload)

		// IMAGE_RESOURCE_DATA_ENTRY：OffsetToData（稍后重定位）、Size、CodePage、Reserved。
		binary.LittleEndian.PutUint32(dir[leaf.entryOffset:], leaf.payloadOff)
		binary.LittleEndian.PutUint32(dir[leaf.entryOffset+4:], uint32(len(leaf.payload)))
		binary.LittleEndian.PutUint32(dir[leaf.entryOffset+8:], leaf.codepage)
		binary.LittleEndian.PutUint32(dir[leaf.entryOffset+12:], 0)
		relocs = append(relocs, leaf.entryOffset)
	}

	for _, d := range dirs {
		off := d.offset
		binary.LittleEndian.PutUint16(dir[off+12:], 0) // NumberOfNamedEntries
		binary.LittleEndian.PutUint16(dir[off+14:], uint16(len(d.entries)))
		for i, entry := range d.entries {
			p := off + 16 + uint32(8*i)
			binary.LittleEndian.PutUint32(dir[p:], entry.id)
			if entry.dir != nil {
				binary.LittleEndian.PutUint32(dir[p+4:], 0x80000000|entry.dir.offset)
			} else {
				binary.LittleEndian.PutUint32(dir[p+4:], entry.data.entryOffset)
			}
		}
	}
	return dir, dataBuf.Bytes(), relocs
}

// ---------- COFF 目标文件 ----------

func machineFor(arch Arch) (uint16, uint16, bool) {
	// 第二个返回值是 IMAGE_REL_*_ADDR32NB，即「32 位地址、不带基址」的重定位。
	switch arch {
	case Arch386:
		return 0x014c, 0x0007, true
	case ArchAMD64:
		return 0x8664, 0x0003, true
	case ArchARM64:
		return 0xaa64, 0x0002, true
	default:
		return 0, 0, false
	}
}

// coffObject 把资源目录和数据区写成一个 COFF 目标文件。
// 两个节的名字必须是 .rsrc$01 / .rsrc$02：Go 链接器靠这两个名字
// 认出它们是 Windows 资源，并直接拼进 exe 的资源目录。
func coffObject(arch Arch, dir, payload []byte, relocs []uint32) []byte {
	machine, relocType, _ := machineFor(arch)
	dir = pad4(dir)
	payload = pad4(payload)

	const fileHeaderSize = 20
	const sectionHeaderSize = 40
	const symbolSize = 18

	relocBytes := 10 * len(relocs)
	offset := fileHeaderSize + 2*sectionHeaderSize
	dirPtr := offset
	offset += len(dir)
	relocPtr := offset
	offset += relocBytes
	offset = int(align(uint32(offset), 4))
	payloadPtr := offset
	if len(payload) == 0 {
		payloadPtr = 0
	}
	offset += len(payload)
	offset = int(align(uint32(offset), 4))
	symPtr := offset
	offset += 2 * symbolSize
	// COFF 的字符串表即使为空，也要有一个 4 字节的长度字段。
	strTableSize := 4
	out := make([]byte, offset+strTableSize)

	binary.LittleEndian.PutUint16(out[0:], machine)
	binary.LittleEndian.PutUint16(out[2:], 2) // NumberOfSections
	binary.LittleEndian.PutUint32(out[4:], 0) // TimeDateStamp 固定为 0，产物可复现
	binary.LittleEndian.PutUint32(out[8:], uint32(symPtr))
	binary.LittleEndian.PutUint32(out[12:], 2) // NumberOfSymbols
	binary.LittleEndian.PutUint16(out[16:], 0) // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(out[18:], 0) // Characteristics

	dirHeader := out[fileHeaderSize : fileHeaderSize+sectionHeaderSize]
	payloadHeader := out[fileHeaderSize+sectionHeaderSize : fileHeaderSize+2*sectionHeaderSize]
	writeSectionHeader(dirHeader, ".rsrc$01", uint32(len(dir)), uint32(dirPtr), uint32(len(relocs)), uint32(relocPtr))
	writeSectionHeader(payloadHeader, ".rsrc$02", uint32(len(payload)), uint32(payloadPtr), 0, 0)

	copy(out[dirPtr:], dir)
	copy(out[payloadPtr:], payload)

	for i, rva := range relocs {
		p := relocPtr + 10*i
		binary.LittleEndian.PutUint32(out[p:], rva)
		binary.LittleEndian.PutUint32(out[p+4:], 1) // 指向第二个符号 .rsrc$02
		binary.LittleEndian.PutUint16(out[p+8:], relocType)
	}

	writeSectionSymbol(out[symPtr:], ".rsrc$01", 1)
	writeSectionSymbol(out[symPtr+symbolSize:], ".rsrc$02", 2)

	binary.LittleEndian.PutUint32(out[len(out)-4:], 4) // 字符串表长度只含它自己
	return out
}

func writeSectionHeader(dst []byte, name string, size, rawPtr, numRelocs, relocPtr uint32) {
	copy(dst[0:8], name)
	binary.LittleEndian.PutUint32(dst[8:], 0)  // VirtualSize
	binary.LittleEndian.PutUint32(dst[12:], 0) // VirtualAddress
	binary.LittleEndian.PutUint32(dst[16:], size)
	binary.LittleEndian.PutUint32(dst[20:], rawPtr)
	binary.LittleEndian.PutUint32(dst[24:], relocPtr)
	binary.LittleEndian.PutUint32(dst[28:], 0) // PointerToLinenumbers
	binary.LittleEndian.PutUint16(dst[32:], uint16(numRelocs))
	binary.LittleEndian.PutUint16(dst[34:], 0)          // NumberOfLinenumbers
	binary.LittleEndian.PutUint32(dst[36:], 0x40000040) // 已初始化数据 + 可读
}

func writeSectionSymbol(dst []byte, name string, section int16) {
	copy(dst[0:8], name) // 名字正好 8 字节，不需要字符串表
	binary.LittleEndian.PutUint32(dst[8:], 0)
	binary.LittleEndian.PutUint16(dst[12:], uint16(section))
	binary.LittleEndian.PutUint16(dst[14:], 0) // Type
	dst[16] = 3                                // IMAGE_SYM_CLASS_STATIC
	dst[17] = 0                                // NumberOfAuxSymbols
}

// ---------- 版本信息 ----------

// versionBlock 是一段 VS_VERSIONINFO 结构。Windows 的版本信息是
// 层层嵌套的「长度 + 键名 + 值 + 子块」结构，这里按同样的规则拼。
type versionBlock struct {
	key         string
	typ         uint16
	value       []byte
	valueLength uint16
	children    []versionBlock
}

func (b versionBlock) bytes() []byte {
	out := make([]byte, 6)
	out = append(out, utf16LE(b.key+"\x00")...)
	out = pad4(out)
	out = append(out, b.value...)
	out = pad4(out)
	for _, child := range b.children {
		out = append(out, child.bytes()...)
	}
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], b.valueLength)
	binary.LittleEndian.PutUint16(out[4:], b.typ)
	return out
}

func versionInfo(opt Options) []byte {
	fileMS, fileLS := versionHalves(opt.FileVersion)
	prodMS, prodLS := versionHalves(opt.ProductVersion)

	fixed := make([]byte, 52)
	binary.LittleEndian.PutUint32(fixed[0:], 0xFEEF04BD)
	binary.LittleEndian.PutUint32(fixed[4:], 0x00010000)
	binary.LittleEndian.PutUint32(fixed[8:], fileMS)
	binary.LittleEndian.PutUint32(fixed[12:], fileLS)
	binary.LittleEndian.PutUint32(fixed[16:], prodMS)
	binary.LittleEndian.PutUint32(fixed[20:], prodLS)
	binary.LittleEndian.PutUint32(fixed[24:], 0x3F)    // FileFlagsMask
	binary.LittleEndian.PutUint32(fixed[28:], 0)       // FileFlags
	binary.LittleEndian.PutUint32(fixed[32:], 0x40004) // VOS_NT_WINDOWS32
	binary.LittleEndian.PutUint32(fixed[36:], 1)       // VFT_APP
	binary.LittleEndian.PutUint32(fixed[40:], 0)       // FileSubtype
	binary.LittleEndian.PutUint32(fixed[44:], 0)       // FileDateMS
	binary.LittleEndian.PutUint32(fixed[48:], 0)       // FileDateLS

	fields := []struct{ key, value string }{
		{"CompanyName", opt.CompanyName},
		{"FileDescription", opt.FileDescription},
		{"FileVersion", opt.FileVersion},
		{"InternalName", opt.InternalName},
		{"LegalCopyright", opt.Copyright},
		{"OriginalFilename", opt.OriginalFilename},
		{"ProductName", opt.ProductName},
		{"ProductVersion", opt.ProductVersion},
	}
	var stringBlocks []versionBlock
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		stringBlocks = append(stringBlocks, versionBlock{
			key:         field.key,
			typ:         1,
			value:       utf16LE(field.value + "\x00"),
			valueLength: uint16(len(utf16.Encode([]rune(field.value))) + 1),
		})
	}

	lang := opt.Language
	codepage := opt.Codepage
	stringTable := versionBlock{
		key:      fmt.Sprintf("%04X%04X", lang, codepage),
		typ:      1,
		children: stringBlocks,
	}
	stringFileInfo := versionBlock{key: "StringFileInfo", typ: 1, children: []versionBlock{stringTable}}
	translation := versionBlock{
		key:         "Translation",
		typ:         0,
		value:       []byte{byte(lang), byte(lang >> 8), byte(codepage), byte(codepage >> 8)},
		valueLength: 4,
	}
	varFileInfo := versionBlock{key: "VarFileInfo", typ: 1, children: []versionBlock{translation}}

	root := versionBlock{
		key:         "VS_VERSION_INFO",
		typ:         0,
		value:       fixed,
		valueLength: uint16(len(fixed)),
		children:    []versionBlock{stringFileInfo, varFileInfo},
	}
	return root.bytes()
}

// versionHalves 把 "1.2.3.4" 折成固定信息的两个 32 位整数。
func versionHalves(version string) (uint32, uint32) {
	parts := versionParts(version)
	return parts[0]<<16 | parts[1], parts[2]<<16 | parts[3]
}

// versionParts 把 "1.2.3" 补成四段数字，非数字的段按 0 处理。
func versionParts(version string) [4]uint32 {
	parts := [4]uint32{}
	for i, piece := range strings.Split(strings.TrimSpace(version), ".") {
		if i >= 4 {
			break
		}
		n, err := strconv.ParseUint(strings.TrimSpace(piece), 10, 16)
		if err != nil {
			continue
		}
		parts[i] = uint32(n)
	}
	return parts
}

// fourPartVersion 把 "1.2.3" 写成清单要求的 "1.2.3.0"。
func fourPartVersion(version string) string {
	parts := versionParts(version)
	return fmt.Sprintf("%d.%d.%d.%d", parts[0], parts[1], parts[2], parts[3])
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[2*i:], unit)
	}
	return out
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func align(v, n uint32) uint32 {
	return (v + n - 1) / n * n
}
