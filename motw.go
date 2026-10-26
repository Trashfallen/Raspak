package main

import "os"

// Mark of the Web: Windows помечает скачанные файлы потоком Zone.Identifier.
// Проводник при распаковке zip переносит пометку на файлы - делаем так же,
// иначе exe и документы из скачанного архива открываются без SmartScreen
// и без защищенного просмотра Office.

const zoneStream = ":Zone.Identifier"

// partSuffix - временный файл при замене существующего.
const partSuffix = ".raspak-part"

// readZone возвращает пометку архива или nil, если архив не из интернета.
func readZone(archive string) []byte {
	b, err := os.ReadFile(archive + zoneStream)
	if err != nil || len(b) == 0 || len(b) > 64<<10 {
		return nil
	}
	return b
}

// markZone ставит пометку на распакованный файл. Ошибку не считаем: на FAT
// и сетевых дисках потоков нет, а распаковку это ломать не должно.
func markZone(target string, zone []byte) {
	_ = os.WriteFile(target+zoneStream, zone, 0o666)
}
