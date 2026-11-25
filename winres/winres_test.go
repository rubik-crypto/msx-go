package winres

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// fakeExe 模拟 Go 链接器的做法：把 .rsrc$01 和 .rsrc$02 依次拼成 exe 里的资源节，
// 并把目录中记录数据偏移的位置改写成「数据在整节里的偏移」。
// 这样测试读到的就是 Windows 真正会看到的东西。
func fakeExe(t *testing.T, obj []byte) []byte {
	t.Helper()
	if len(obj) < 20 {
		t.Fatal("目标文件太短")
	}
	if got := binary.LittleEndian.Uint16(obj[0:]); got != 0x8664 {
		t.Fatalf("机器类型应为 amd64，实际 %#x", got)
	}
	if got := binary.LittleEndian.Uint16(obj[2:]); got != 2 {
		t.Fatalf("应该有两个节，实际 %d", got)
	}
	sect := func(i int) (name string, size, raw, numRelocs, relocPtr uint32) {
		h := obj[20+40*i : 20+40*(i+1)]
		return strings.TrimRight(string(h[0:8]), "\x00"),
			binary.LittleEndian.Uint32(h[16:]),
			binary.LittleEndian.Uint32(h[20:]),
			uint32(binary.LittleEndian.Uint16(h[32:])),
			binary.LittleEndian.Uint32(h[24:])
	}
	name1, size1, raw1, nrel1, rel1 := sect(0)
	name2, size2, raw2, _, _ := sect(1)
	if name1 != ".rsrc$01" || name2 != ".rsrc$02" {
		t.Fatalf("节名不对: %q / %q", name1, name2)
	}
	dir := append([]byte(nil), obj[raw1:raw1+size1]...)
	payload := append([]byte(nil), obj[raw2:raw2+size2]...)
	if nrel1 == 0 {
		t.Fatal("资源目录里没有重定位，链接器不会改写数据偏移")
	}
	out := append(dir, payload...)
	for i := uint32(0); i < nrel1; i++ {
		p := rel1 + 10*i
		off := binary.LittleEndian.Uint32(obj[p:])
		sym := binary.LittleEndian.Uint32(obj[p+4:])
		typ := binary.LittleEndian.Uint16(obj[p+8:])
		if sym != 1 {
			t.Fatalf("重定位应该指向 .rsrc$02（符号 1），实际 %d", sym)
		}
		if typ != 0x0003 {
			t.Fatalf("重定位类型应为 IMAGE_REL_AMD64_ADDR32NB，实际 %#x", typ)
		}
		addend := binary.LittleEndian.Uint32(dir[off:])
		binary.LittleEndian.PutUint32(out[off:], uint32(size1)+addend)
	}
	return out
}

// readResources 按 Windows 的规则遍历资源目录，返回「类型 → 内容」。
func readResources(t *testing.T, section []byte) map[uint32][]byte {
	t.Helper()
	u32 := func(off uint32) uint32 {
		if int(off)+4 > len(section) {
			t.Fatalf("资源目录越界: 偏移 %d，长度 %d", off, len(section))
		}
		return binary.LittleEndian.Uint32(section[off:])
	}
	dirEntries := func(off uint32) []struct {
		id  uint32
		sub bool
		off uint32
	} {
		if int(off)+16 > len(section) {
			t.Fatalf("目录头越界: 偏移 %d", off)
		}
		count := int(binary.LittleEndian.Uint16(section[off+12:])) +
			int(binary.LittleEndian.Uint16(section[off+14:]))
		out := make([]struct {
			id  uint32
			sub bool
			off uint32
		}, 0, count)
		for i := 0; i < count; i++ {
			base := off + 16 + uint32(8*i)
			raw := u32(base + 4)
			out = append(out, struct {
				id  uint32
				sub bool
				off uint32
			}{id: u32(base), sub: raw&0x80000000 != 0, off: raw & 0x7fffffff})
		}
		return out
	}

	out := map[uint32][]byte{}
	for _, typ := range dirEntries(0) {
		if !typ.sub {
			t.Fatalf("类型 %d 下面应该是子目录", typ.id)
		}
		for _, name := range dirEntries(typ.off) {
			if !name.sub {
				t.Fatalf("资源名 %d 下面应该是语言子目录", name.id)
			}
			for _, lang := range dirEntries(name.off) {
				if lang.sub {
					t.Fatalf("语言项下面不该再有子目录")
				}
				dataOff := u32(lang.off)
				size := u32(lang.off + 4)
				if int(dataOff)+int(size) > len(section) {
					t.Fatalf("资源内容越界: 偏移 %d 大小 %d，节长 %d", dataOff, size, len(section))
				}
				out[typ.id] = append([]byte(nil), section[dataOff:dataOff+size]...)
			}
		}
	}
	return out
}

func TestObjectWritesVersionAndManifest(t *testing.T) {
	obj, err := Object(ArchAMD64, Options{
		FileVersion:    "1.8.0",
		ProductName:    "示例程序",
		ProductVersion: "1.8.0",
		AssemblyName:   "SampleApp",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := readResources(t, fakeExe(t, obj))

	manifest, ok := res[rtManifest]
	if !ok {
		t.Fatalf("exe 里没有使用清单，只有 %v", res)
	}
	text := string(manifest)
	for _, want := range []string{
		`name="SampleApp"`,
		`version="1.8.0.0"`,
		`level="asInvoker"`,
		`{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}`, // Windows 10 / 11
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("清单里缺少 %q：\n%s", want, text)
		}
	}

	version, ok := res[rtVersion]
	if !ok {
		t.Fatalf("exe 里没有版本信息，只有 %v", res)
	}
	signature := []byte{0xBD, 0x04, 0xEF, 0xFE}
	at := bytes.Index(version, signature)
	if at < 0 || at+52 > len(version) {
		t.Fatal("版本信息里找不到 VS_FIXEDFILEINFO")
	}
	for _, want := range []string{"VS_VERSION_INFO", "StringFileInfo", "ProductName", "FileVersion"} {
		if !bytes.Contains(version, utf16LE(want)) {
			t.Fatalf("版本信息里缺少 %q", want)
		}
	}
	// 1.8.0 → 1.8 / 0.0
	if got := binary.LittleEndian.Uint32(version[at+8:]); got != 1<<16|8 {
		t.Fatalf("文件版本高 32 位不对: %#x", got)
	}
	if got := binary.LittleEndian.Uint32(version[at+12:]); got != 0 {
		t.Fatalf("文件版本低 32 位不对: %#x", got)
	}
	// 版本信息是 UTF-16，中文必须按 UTF-16 存进去，不能变成问号。
	if !bytes.Contains(version, utf16LE("示例程序")) {
		t.Fatal("中文产品名没有按 UTF-16 编码写入版本信息")
	}
}

func TestObjectIsReproducible(t *testing.T) {
	opt := Options{FileVersion: "1.8.0", ProductName: "示例程序"}
	first, err := Object(ArchAMD64, opt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Object(ArchAMD64, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("同样的输入两次生成的资源文件不一样")
	}
}

func TestObjectRejectsUnknownArch(t *testing.T) {
	if _, err := Object(Arch("mips"), Options{FileVersion: "1.0.0"}); err == nil {
		t.Fatal("未知架构应该报错")
	}
	for _, arch := range []Arch{Arch386, ArchAMD64, ArchARM64} {
		if _, err := Object(arch, Options{FileVersion: "1.0.0"}); err != nil {
			t.Fatalf("%s 应该能生成: %v", arch, err)
		}
	}
}

func TestVersionFormatting(t *testing.T) {
	cases := map[string][2]uint32{
		"1.8.0":   {1<<16 | 8, 0},
		"2.3.4.5": {2<<16 | 3, 4<<16 | 5},
		"7":       {7 << 16, 0},
		"":        {0, 0},
		"x.y":     {0, 0},
	}
	for input, want := range cases {
		ms, ls := versionHalves(input)
		if ms != want[0] || ls != want[1] {
			t.Fatalf("%q 折成 %#x %#x，应为 %#x %#x", input, ms, ls, want[0], want[1])
		}
	}
	if got := fourPartVersion("1.8.0"); got != "1.8.0.0" {
		t.Fatalf("四段版本号不对: %q", got)
	}
}
