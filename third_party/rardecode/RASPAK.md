# rardecode для Raspak

Копия github.com/nwaples/rardecode/v2 v2.4.6 (лицензия BSD-2, LICENSE) с одной правкой.

`decode_reader.go`: фильтр (E8, delta и др.), который декодер ставит в очередь,
пока `bufBytes` еще собирает блок предыдущего фильтра, считался от уже
сдвинутой позиции чтения, а список фильтров считается от начала этого блока.
Смещение уезжало назад: solid-архивы WinRAR 7 (метод v6 у 7-Zip) не открывались
с «rardecode: invalid filter» или «decoder expected more data than is in packed file».
Теперь учитываются байты блока, уже взятые из окна (`pend`).

Тест: `filter_pend_test.go`.
