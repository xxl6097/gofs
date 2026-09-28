#!/usr/bin/env python3
"""为 gofs 生成端到端测试用的目录与压缩包。"""
import io
import os
import shutil
import tarfile
import zipfile
import gzip

ROOT = "/tmp/gofs-test"
shutil.rmtree(ROOT, ignore_errors=True)
os.makedirs(ROOT, exist_ok=True)


def w(path, data):
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "wb") as f:
        f.write(data if isinstance(data, bytes) else data.encode())


# ---- 普通文件 ----
w("docs/readme.md", "# gofs 测试文档\n\n这是普通 Markdown 文件。\n")
w("docs/guide.txt", "使用说明\n" + "内容行\n" * 200)
w("中文目录/中文文件.txt", "中文内容测试\n")
w("notes.txt", "顶层文件\n")

# ---- 一个稍大的文件，用于验证压缩与进度 ----
w("docs/big.bin", bytes(range(256)) * 4096)  # 1 MiB

# ---- sample.zip ----
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("hello.txt", "来自 zip 的问候\n")
    z.writestr("sub/nested/深.txt", "嵌套目录里的中文内容\n")
    z.writestr("sub/big.bin", bytes(range(256)) * 2048)
    z.writestr("empty-dir/", "")
open(os.path.join(ROOT, "sample.zip"), "wb").write(buf.getvalue())

# ---- sample.tar.gz ----
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w:gz") as t:
    for name, data in [
        ("a.txt", b"tar.gz content A\n"),
        ("deep/dir/b.txt", b"tar.gz content B\n"),
        ("中文名.txt", "中文内容 in tar.gz\n".encode()),
    ]:
        info = tarfile.TarInfo(name)
        info.size = len(data)
        t.addfile(info, io.BytesIO(data))
open(os.path.join(ROOT, "sample.tar.gz"), "wb").write(buf.getvalue())

# ---- sample.tar ----
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w") as t:
    for name, data in [("plain.txt", b"uncompressed tar\n"), ("x/y.txt", b"deep\n")]:
        info = tarfile.TarInfo(name)
        info.size = len(data)
        t.addfile(info, io.BytesIO(data))
open(os.path.join(ROOT, "sample.tar"), "wb").write(buf.getvalue())

# ---- notes.txt.gz ----
with gzip.open(os.path.join(ROOT, "notes.txt.gz"), "wb") as f:
    f.write("这是被 gzip 压缩的纯文本。\n".encode() * 50)

# ---- evil.zip：路径穿越 / 绝对路径 / 软链接 / 炸弹 ----
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("../../escaped-relative.txt", "试图用 .. 逃逸\n")
    z.writestr("/tmp/gofs-escaped-absolute.txt", "试图用绝对路径逃逸\n")
    # 解压炸弹：高压缩比
    z.writestr("bomb.bin", b"\0" * (40 * 1024 * 1024))
open(os.path.join(ROOT, "evil.zip"), "wb").write(buf.getvalue())

# ---- evil.tar.gz：含软链接 ----
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w:gz") as t:
    data = b"normal\n"
    info = tarfile.TarInfo("ok.txt")
    info.size = len(data)
    t.addfile(info, io.BytesIO(data))
    # 指向 /etc/passwd 的软链接
    link = tarfile.TarInfo("passwd-link")
    link.type = tarfile.SYMTYPE
    link.linkname = "/etc/passwd"
    t.addfile(link)
    # 逃逸路径
    esc = tarfile.TarInfo("../../tar-escaped.txt")
    esc.size = 4
    t.addfile(esc, io.BytesIO(b"esc\n"))
open(os.path.join(ROOT, "evil.tar.gz"), "wb").write(buf.getvalue())

# ---- 加密 zip（用于验证友好报错）----
print("生成完成，目录内容：")
for dirpath, dirnames, filenames in os.walk(ROOT):
    for fn in sorted(filenames):
        p = os.path.join(dirpath, fn)
        print("  %-46s %8d" % (os.path.relpath(p, ROOT), os.path.getsize(p)))
