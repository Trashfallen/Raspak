package main

// Регистрация пункта в новом меню Windows 11 (обработчик - comserver.go).
//
// Программа сама собирает пакет MSIX без файлов программы (sparse package):
// манифест с расширением меню проводника и COM-сервером + картинки. Файлы
// программы остаются на месте: пакет ссылается на папку с Raspak.exe
// (-ExternalLocation). Ставится для текущего пользователя.
//
// Неподписанный пакет Windows в меню не пускает («неподписанный пакет не
// может включать исполняемые активации»), поэтому на компьютере создается
// свой самоподписанный сертификат, пакет подписывается им (msixsign.go), а
// закрытый ключ сразу удаляется - больше им ничего не подписать. Открытая
// часть добавляется в «Доверенные лица» пользователя, а если Windows этого
// мало - компьютера (один запрос UAC). Подписанный пакет хранится и
// переиспользуется, пока не меняется манифест (menuPkgVersion) или имя exe.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"raspak/internal/appicon"
)

const (
	menuPkgName      = "Raspak"
	menuPkgPublisher = "CN=Raspak Local"
	// menuPkgVersion поднимать при любом изменении манифеста или пунктов меню
	menuPkgVersion = "1.0.0.0"
)

// win11Menu: в этой Windows есть новое контекстное меню.
func win11Menu() bool {
	return windows.RtlGetVersion().BuildNumber >= 22000
}

func menuManifest(exeName, publisher string) string {
	clsid := strings.Trim(menuCLSID.String(), "{}")
	var types strings.Builder
	for _, ext := range archiveExts {
		fmt.Fprintf(&types, "            <desktop5:ItemType Type=%q><desktop5:Verb Id=\"Raspak\" Clsid=%q/></desktop5:ItemType>\n", ext, clsid)
	}
	return `<?xml version="1.0" encoding="utf-8"?>
<Package xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10"
  xmlns:uap="http://schemas.microsoft.com/appx/manifest/uap/windows10"
  xmlns:uap10="http://schemas.microsoft.com/appx/manifest/uap/windows10/10"
  xmlns:desktop4="http://schemas.microsoft.com/appx/manifest/desktop/windows10/4"
  xmlns:desktop5="http://schemas.microsoft.com/appx/manifest/desktop/windows10/5"
  xmlns:desktop6="http://schemas.microsoft.com/appx/manifest/desktop/windows10/6"
  xmlns:rescap="http://schemas.microsoft.com/appx/manifest/foundation/windows10/restrictedcapabilities"
  xmlns:com="http://schemas.microsoft.com/appx/manifest/com/windows10"
  IgnorableNamespaces="uap uap10 desktop4 desktop5 desktop6 rescap com">
  <Identity Name="` + menuPkgName + `" Publisher="` + publisher + `" Version="` + menuPkgVersion + `" ProcessorArchitecture="neutral"/>
  <Properties>
    <DisplayName>Raspak</DisplayName>
    <PublisherDisplayName>Raspak</PublisherDisplayName>
    <Logo>Assets\StoreLogo.png</Logo>
    <uap10:AllowExternalContent>true</uap10:AllowExternalContent>
    <desktop6:FileSystemWriteVirtualization>disabled</desktop6:FileSystemWriteVirtualization>
    <desktop6:RegistryWriteVirtualization>disabled</desktop6:RegistryWriteVirtualization>
  </Properties>
  <Resources>
    <Resource Language="ru-ru"/>
  </Resources>
  <Dependencies>
    <TargetDeviceFamily Name="Windows.Desktop" MinVersion="10.0.19041.0" MaxVersionTested="10.0.26100.0"/>
  </Dependencies>
  <Capabilities>
    <rescap:Capability Name="runFullTrust"/>
    <rescap:Capability Name="unvirtualizedResources"/>
  </Capabilities>
  <Applications>
    <Application Id="Raspak" Executable="` + exeName + `" uap10:TrustLevel="mediumIL" uap10:RuntimeBehavior="win32App">
      <uap:VisualElements AppListEntry="none" DisplayName="Raspak" Description="Распаковщик архивов"
        BackgroundColor="transparent" Square150x150Logo="Assets\Square150x150Logo.png" Square44x44Logo="Assets\Square44x44Logo.png"/>
      <Extensions>
        <desktop4:Extension Category="windows.fileExplorerContextMenus">
          <desktop4:FileExplorerContextMenus>
` + types.String() + `          </desktop4:FileExplorerContextMenus>
        </desktop4:Extension>
        <com:Extension Category="windows.comServer">
          <com:ComServer>
            <com:ExeServer Executable="` + exeName + `" Arguments="--com-server" DisplayName="Raspak">
              <com:Class Id="` + clsid + `" DisplayName="Raspak"/>
            </com:ExeServer>
          </com:ComServer>
        </com:Extension>
      </Extensions>
    </Application>
  </Applications>
</Package>
`
}

// buildMenuPackage пишет .msix: файлы без сжатия + AppxBlockMap.xml (SHA-256
// каждого блока 64 КБ) + [Content_Types].xml, без подписи. Zip пишется вручную
// так же, как у makeappx: версии 20/45 и обязательная запись Zip64 в конце -
// стандартный archive/zip Windows не принимает.
func buildMenuPackage(path, exeName, publisher string) error {
	parts := []struct {
		name string
		data []byte
	}{
		{"AppxManifest.xml", []byte(menuManifest(exeName, publisher))},
		{"Assets/StoreLogo.png", appicon.PNG(50)},
		{"Assets/Square150x150Logo.png", appicon.PNG(150)},
		{"Assets/Square44x44Logo.png", appicon.PNG(44)},
	}
	var bm strings.Builder
	bm.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="no"?>` + "\n")
	bm.WriteString(`<BlockMap xmlns="http://schemas.microsoft.com/appx/2010/blockmap" HashMethod="http://www.w3.org/2001/04/xmlenc#sha256">` + "\n")
	for _, p := range parts {
		fmt.Fprintf(&bm, `<File Name="%s" Size="%d" LfhSize="%d">`, strings.ReplaceAll(p.name, "/", `\`), len(p.data), 30+len(p.name))
		for off := 0; off < len(p.data); off += 64 << 10 {
			sum := sha256.Sum256(p.data[off:min(off+64<<10, len(p.data))])
			fmt.Fprintf(&bm, `<Block Hash="%s"/>`, base64.StdEncoding.EncodeToString(sum[:]))
		}
		bm.WriteString("</File>\n")
	}
	bm.WriteString("</BlockMap>\n")
	ct := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="png" ContentType="image/png"/>` +
		`<Default Extension="xml" ContentType="application/vnd.ms-appx.manifest+xml"/>` +
		`<Override PartName="/AppxBlockMap.xml" ContentType="application/vnd.ms-appx.blockmap+xml"/>` +
		`</Types>`
	parts = append(parts, struct {
		name string
		data []byte
	}{"AppxBlockMap.xml", []byte(bm.String())}, struct {
		name string
		data []byte
	}{"[Content_Types].xml", []byte(ct)})

	var out, cd bytes.Buffer
	le := func(b *bytes.Buffer, vs ...any) {
		for _, v := range vs {
			_ = binary.Write(b, binary.LittleEndian, v)
		}
	}
	const dosTime, dosDate = uint16(0), uint16(0x5821) // 01.01.2024 00:00
	for _, p := range parts {
		off := uint32(out.Len())
		crc := crc32.ChecksumIEEE(p.data)
		size := uint32(len(p.data))
		le(&out, uint32(0x04034b50), uint16(20), uint16(0), uint16(0), dosTime, dosDate, crc, size, size, uint16(len(p.name)), uint16(0))
		out.WriteString(p.name)
		out.Write(p.data)
		le(&cd, uint32(0x02014b50), uint16(45), uint16(20), uint16(0), uint16(0), dosTime, dosDate, crc, size, size,
			uint16(len(p.name)), uint16(0), uint16(0), uint16(0), uint16(0), uint32(0), off)
		cd.WriteString(p.name)
	}
	cdOff, cdSize, n := uint64(out.Len()), uint64(cd.Len()), uint64(len(parts))
	out.Write(cd.Bytes())
	z64 := uint64(out.Len())
	le(&out, uint32(0x06064b50), uint64(44), uint16(45), uint16(45), uint32(0), uint32(0), n, n, cdSize, cdOff)
	le(&out, uint32(0x07064b50), uint32(0), z64, uint32(1))
	le(&out, uint32(0x06054b50), uint16(0), uint16(0), uint16(0xFFFF), uint16(0xFFFF), uint32(0xFFFFFFFF), uint32(0xFFFFFFFF), uint16(0))
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// powershell выполняет скрипт без окна и возвращает вывод.
func powershell(script string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if len(text) > 600 {
			text = text[:600]
		}
		return text, fmt.Errorf("%v: %s", err, text)
	}
	return text, nil
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// registerMenuPackage ставит (или переставляет) пакет меню для папки с exe.
func registerMenuPackage(exe string) error {
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	pkg := filepath.Join(dir, "Raspak-menu.msix")
	cer := filepath.Join(dir, "Raspak-menu.cer")
	stampFile := filepath.Join(dir, "Raspak-menu.txt")
	stamp := filepath.Base(exe) + "|" + menuPkgVersion
	if b, _ := os.ReadFile(stampFile); string(b) != stamp || !fileExists(pkg) || !fileExists(cer) {
		if err := makeSignedPackage(pkg, cer, filepath.Base(exe)); err != nil {
			return err
		}
		_ = os.WriteFile(stampFile, []byte(stamp), 0o644)
	}
	// прежний пакет (другая папка exe) надо снять, иначе Windows откажет
	install := "$ErrorActionPreference='Stop';" +
		"Get-AppxPackage -Name " + menuPkgName + " | Remove-AppxPackage;" +
		"Add-AppxPackage -Path " + psQuote(pkg) + " -ExternalLocation " + psQuote(filepath.Dir(exe))
	trustUser := "Import-Certificate -FilePath " + psQuote(cer) + ` -CertStoreLocation Cert:\CurrentUser\TrustedPeople | Out-Null;`
	_, err := powershell(trustUser + install)
	if err != nil && strings.Contains(err.Error(), "800B01") {
		// сертификат не признан: Windows доверяет только хранилищу компьютера
		if err := trustCertMachine(cer); err != nil {
			return err
		}
		_, err = powershell(install)
	}
	if err != nil {
		return fmt.Errorf("регистрация меню Windows 11: %w", err)
	}
	return nil
}

// makeSignedPackage: новый сертификат, пакет, подпись, удаление закрытого ключа.
func makeSignedPackage(pkg, cer, exeName string) error {
	out, err := powershell("$c = New-SelfSignedCertificate -Type Custom -Subject " + psQuote(menuPkgPublisher) +
		" -FriendlyName 'Raspak: меню проводника' -KeyUsage DigitalSignature -KeyAlgorithm RSA -KeyLength 2048" +
		` -NotAfter (Get-Date).AddYears(50) -CertStoreLocation Cert:\CurrentUser\My` +
		" -TextExtension @('2.5.29.37={text}1.3.6.1.5.5.7.3.3','2.5.29.19={text}');" +
		"Export-Certificate -Cert $c -FilePath " + psQuote(cer) + " | Out-Null; $c.Thumbprint")
	if err != nil {
		return fmt.Errorf("сертификат для меню: %w", err)
	}
	lines := strings.Fields(out)
	thumb := lines[len(lines)-1]
	defer powershell(`Remove-Item Cert:\CurrentUser\My\` + thumb + " -DeleteKey")
	hash, err := hex.DecodeString(thumb)
	if err != nil || len(hash) != 20 {
		return fmt.Errorf("сертификат для меню: странный отпечаток %q", thumb)
	}
	if err := buildMenuPackage(pkg, exeName, menuPkgPublisher); err != nil {
		return fmt.Errorf("пакет меню: %w", err)
	}
	return signMSIX(pkg, hash)
}

// trustCertMachine добавляет открытый сертификат в «Доверенные лица» компьютера (UAC).
func trustCertMachine(cer string) error {
	_, err := powershell("$p = Start-Process certutil.exe -ArgumentList '-addstore','TrustedPeople'," + psQuote(`"`+cer+`"`) +
		" -Verb RunAs -WindowStyle Hidden -Wait -PassThru; exit $p.ExitCode")
	if err != nil {
		return errors.New("для меню Windows 11 нужно один раз разрешить запрос администратора (UAC)")
	}
	return nil
}

func unregisterMenuPackage() error {
	_, err := powershell("Get-AppxPackage -Name " + menuPkgName + " | Remove-AppxPackage;" +
		`Get-ChildItem Cert:\CurrentUser\TrustedPeople | Where-Object { $_.Subject -eq ` + psQuote(menuPkgPublisher) + " } | Remove-Item")
	for _, f := range []string{"Raspak-menu.msix", "Raspak-menu.cer", "Raspak-menu.txt"} {
		os.Remove(filepath.Join(dataDir(), f))
	}
	return err
}
