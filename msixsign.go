package main

// Подпись пакета MSIX средствами Windows (mssign32.dll, SignerSignEx2 с
// данными APPX SIP) - так же подписывает SignTool, только без SDK.

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSignerSignEx2 = windows.NewLazySystemDLL("mssign32.dll").NewProc("SignerSignEx2")
var procSignerFreeSignerContext = windows.NewLazySystemDLL("mssign32.dll").NewProc("SignerFreeSignerContext")

type signerFileInfo struct {
	cbSize   uint32
	fileName *uint16
	file     windows.Handle
}

type signerSubjectInfo struct {
	cbSize        uint32
	index         *uint32
	subjectChoice uint32
	fileInfo      *signerFileInfo
}

type signerCertStoreInfo struct {
	cbSize      uint32
	signingCert *windows.CertContext
	certPolicy  uint32
	certStore   windows.Handle
}

type signerCert struct {
	cbSize     uint32
	certChoice uint32
	storeInfo  *signerCertStoreInfo
	hwnd       uintptr
}

type signerSignatureInfo struct {
	cbSize          uint32
	algidHash       uint32
	attrChoice      uint32
	attrAuthcode    uintptr
	authenticated   uintptr
	unauthenticated uintptr
}

type signerSignEx2Params struct {
	flags          uint32
	subjectInfo    *signerSubjectInfo
	signingCert    *signerCert
	signatureInfo  *signerSignatureInfo
	providerInfo   uintptr
	timestampFlags uint32
	algorithmOid   *byte
	timestampURL   *uint16
	cryptAttrs     uintptr
	sipData        *appxSipClientData
	signerContext  *uintptr
	cryptoPolicy   uintptr
	reserved       uintptr
}

type appxSipClientData struct {
	signerParams *signerSignEx2Params
	appxSipState unsafe.Pointer // IUnknown, освобождаем после подписи
}

// signMSIX подписывает пакет сертификатом с данным отпечатком SHA-1 из
// хранилища «Личные» текущего пользователя.
func signMSIX(path string, thumbprint []byte) error {
	store, err := windows.CertOpenSystemStore(0, ptr("MY"))
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	blob := windows.CryptHashBlob{Size: uint32(len(thumbprint)), Data: &thumbprint[0]}
	const certFindHash = 0x10000
	cert, err := windows.CertFindCertificateInStore(store, windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		0, certFindHash, unsafe.Pointer(&blob), nil)
	if err != nil {
		return fmt.Errorf("сертификат для подписи не найден: %w", err)
	}
	defer windows.CertFreeCertificateContext(cert)

	const (
		signerSubjectFile     = 1
		signerCertStore       = 2
		signerCertPolicyChain = 8 // SIGNER_CERT_POLICY_CHAIN_NO_ROOT
		calgSHA256            = 0x800c
		signerNoAttr          = 0
	)
	var index uint32
	fileInfo := signerFileInfo{fileName: ptr(path)}
	fileInfo.cbSize = uint32(unsafe.Sizeof(fileInfo))
	subject := signerSubjectInfo{index: &index, subjectChoice: signerSubjectFile, fileInfo: &fileInfo}
	subject.cbSize = uint32(unsafe.Sizeof(subject))
	storeInfo := signerCertStoreInfo{signingCert: cert, certPolicy: signerCertPolicyChain}
	storeInfo.cbSize = uint32(unsafe.Sizeof(storeInfo))
	sc := signerCert{certChoice: signerCertStore, storeInfo: &storeInfo}
	sc.cbSize = uint32(unsafe.Sizeof(sc))
	sig := signerSignatureInfo{algidHash: calgSHA256, attrChoice: signerNoAttr}
	sig.cbSize = uint32(unsafe.Sizeof(sig))

	var ctx uintptr
	params := signerSignEx2Params{subjectInfo: &subject, signingCert: &sc, signatureInfo: &sig, signerContext: &ctx}
	sip := appxSipClientData{signerParams: &params}
	params.sipData = &sip

	hr, _, _ := procSignerSignEx2.Call(0, uintptr(unsafe.Pointer(&subject)), uintptr(unsafe.Pointer(&sc)),
		uintptr(unsafe.Pointer(&sig)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&sip)), uintptr(unsafe.Pointer(&ctx)), 0, 0)
	runtime.KeepAlive(fileInfo.fileName)
	if sip.appxSipState != nil {
		comCall(sip.appxSipState, vRelease)
	}
	if ctx != 0 {
		procSignerFreeSignerContext.Call(ctx)
	}
	if int32(hr) < 0 {
		return errors.New("подпись пакета: " + hresultText(hr))
	}
	return nil
}

func hresultText(hr uintptr) string {
	return fmt.Sprintf("%#x %s", uint32(hr), windows.Errno(hr&0xFFFF).Error())
}
