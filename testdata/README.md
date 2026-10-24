# Тестовые архивы

Сделаны 7-Zip из одного набора файлов:

```
folder/a.txt        "alpha\n"
folder/sub/b.txt    "bravo\n"
folder/rand.bin     5000 случайных байт
root.txt            "root file\n"
папка/файл.txt      "привет\n"   (в zip имя записано в cp866, как делает 7-Zip)
```

- `plain.zip`, `plain.7z`, `plain.tar.gz` - без пароля;
- `secret.7z` (зашифрован и список файлов), `secret-aes.zip` (AES-256), `secret-zc.zip` (ZipCrypto) - пароль `secret`;
- `split.7z.001`-`.003` - тома по 2 КБ;
- `root.txt.gz` - одиночный сжатый файл;
- `multi.part01.rar`, `multi.part02.rar` - многотомный rar из тестов [mholt/archives](https://github.com/mholt/archives) (MIT).
