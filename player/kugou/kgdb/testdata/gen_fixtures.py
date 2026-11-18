# 生成明文 SQLite 夹具（页大小均为 1024）：kgdb 读取器测试用的 basic.db、utf16.db、wal.db，
# 以及 assoc 读库测试用的 ../../assoc/testdata/kugou_like.db、kugou_like_unassociated.db。
#
# 重新生成：在本目录下执行
#     python -I gen_fixtures.py
# 会先删除再重建上述五个文件。内容全部为合成数据，不含任何真实个人数据。
# 文件头里的 SQLite 版本号随生成所用的 sqlite3 库而变，测试不依赖它。

import hashlib
import os
import sqlite3

HERE = os.path.dirname(os.path.abspath(__file__))
ASSOC_TESTDATA = os.path.join(HERE, "..", "..", "assoc", "testdata")

# 设计 §2.2 原文。
LYRIC_INFO_DDL = (
    "CREATE TABLE LyricInfo(Key char(32) PRIMARY KEY NOT NULL,KRCID int,UpdateTime BigInt NOT NULL DEFAULT 0,"
    "LyricPath nvarchar(260) NOT NULL DEFAULT '',SingerNames nvarchar(260),SongName nvarchar(260),"
    "SongHash nvarchar(32),Duration INT NOT NULL DEFAULT 0,RealOffset INT NOT NULL DEFAULT 0,"
    "RecommendOffset INT NOT NULL DEFAULT 0,LastCheckTime BigInt NOT NULL DEFAULT 0,"
    "LyricType INT NOT NULL DEFAULT 0,ExternalInfo nvarchar(1024),AISongID nvarchar(260) NOT NULL DEFAULT '',"
    "Expire BigInt NOT NULL DEFAULT 0,VocabID nvarchar(260) NOT NULL DEFAULT '',"
    "VocabCheckTime BigInt NOT NULL DEFAULT 0)"
)

QUOTED_TABLE_DDL = (
    'CREATE TABLE "Quoted Table"("col a" TEXT, [col b] INTEGER, `col c` REAL, d BLOB, '
    'PRIMARY KEY("col a"), UNIQUE([col b]), CHECK(1))'
)

# 覆盖序列类型 8、9、1、1、2、3、4、5、6。
QUOTED_INTEGERS = [0, 1, -1, 127, 32767, 8388607, 2147483647, 140737488355327, 4611686018427387904]
QUOTED_BLOB = bytes([0x00, 0xFF, 0x7F, 0x80])

# LyricInfo 的两条溢出记录。按 SQLite 文件格式，载荷 P 超过 X = U-35 时本页存放
# K = M+((P-M)%(U-4))（K ≤ X）或 M 字节，U = 1024 时 M = 103：
# 第 7 行 ExternalInfo 3000 字符，P 落在取 M 的一支；第 8 行 SingerNames 1800 字符，P 落在取 K 的一支。
OVERFLOW_ROW = 7
LONG_SINGER_ROW = 8


def md5hex(s):
    return hashlib.md5(s.encode()).hexdigest()


def external_info():
    # "0000,0001,…,0599,"：600 × 5 = 3000 字符，各段不同，溢出页顺序错了能看出来。
    return "".join("%04d," % k for k in range(600))


def long_singer():
    # "0000;0001;…;0359;"：360 × 5 = 1800 字符。
    return "".join("%04d;" % k for k in range(360))


def varint_len(v):
    n = 1
    while v >= 0x80 and n < 9:
        v >>= 7
        n += 1
    return n


def record_size(row):
    # 按记录格式求载荷长度：记录头（自身长度的 varint 与各列序列类型的 varint）加各列的值。
    types = []
    for v in row:
        if v is None:
            types.append((0, 0))
        elif isinstance(v, int):
            if v in (0, 1):
                types.append((8 + v, 0))
            else:
                n = next(n for n in (1, 2, 3, 4, 6, 8) if -(1 << (8 * n - 1)) <= v < 1 << (8 * n - 1))
                types.append(({1: 1, 2: 2, 3: 3, 4: 4, 6: 5, 8: 6}[n], n))
        else:
            b = v.encode() if isinstance(v, str) else v
            types.append(((13 if isinstance(v, str) else 12) + 2 * len(b), len(b)))
    hdr = sum(varint_len(t) for t, _ in types)
    hdr += varint_len(hdr + 1)
    return hdr + sum(n for _, n in types)


def k_branch(p):
    # 本页存放取 K 的一支（见上）。
    u, x, m = 1024, 1024 - 35, (1024 - 12) * 32 // 255 - 23
    return p > x and m + (p - m) % (u - 4) <= x


def pad(i):
    return ("%03d|" % i) * 75  # 300 字符


def fresh(name, directory=HERE):
    path = os.path.join(directory, name)
    for suffix in ("", "-journal", "-wal", "-shm"):
        if os.path.exists(path + suffix):
            os.remove(path + suffix)
    conn = sqlite3.connect(path, isolation_level=None)
    return path, conn


def check_header(path, version, encoding):
    with open(path, "rb") as f:
        h = f.read(100)
    assert int.from_bytes(h[16:18], "big") == 1024, path
    assert h[18] == version and h[19] == version, path
    assert h[20] == 0, path
    assert int.from_bytes(h[56:60], "big") == encoding, path
    for suffix in ("-journal", "-wal", "-shm"):
        assert not os.path.exists(path + suffix), path + suffix


def gen_basic():
    path, conn = fresh("basic.db")
    c = conn.cursor()
    c.execute("PRAGMA page_size=1024")
    c.execute("PRAGMA journal_mode=DELETE")
    c.execute(LYRIC_INFO_DDL)
    c.execute("CREATE TABLE LocalFile(SongID INTEGER PRIMARY KEY, TitleName TEXT, MusicFileHash TEXT, LyricKey TEXT, Pad TEXT)")
    c.execute("CREATE INDEX ix ON LocalFile(MusicFileHash)")
    c.execute(QUOTED_TABLE_DDL)
    c.execute("CREATE TABLE wr(k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID")

    c.execute("BEGIN")
    for i in range(1, 21):
        row = (
            md5hex("lyric-key-%d" % i), 1000 + i, 1700000000000 + i, "Song %02d.krc" % i,
            long_singer() if i == LONG_SINGER_ROW else "Singer %02d" % i,
            "Song %02d" % i, md5hex("song-hash-%d" % i), 180000 + i,
            0, 0, 0, i % 2, external_info() if i == OVERFLOW_ROW else None, "", 0, "", i,
        )
        if i == OVERFLOW_ROW:
            assert record_size(row) > 1024 - 35 and not k_branch(record_size(row)), record_size(row)
        if i == LONG_SINGER_ROW:
            assert k_branch(record_size(row)), record_size(row)
        c.execute("INSERT INTO LyricInfo VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", row)
    c.execute("COMMIT")

    def insert_local(lo, hi):
        c.execute("BEGIN")
        for i in range(lo, hi + 1):
            c.execute(
                "INSERT INTO LocalFile(SongID, TitleName, MusicFileHash, LyricKey, Pad) VALUES(?,?,?,?,?)",
                (i, "Title %03d" % i, md5hex("music-%d" % i), "", pad(i)),
            )
        c.execute("COMMIT")

    insert_local(1, 600)
    c.execute("ALTER TABLE LocalFile ADD COLUMN Extra TEXT DEFAULT 'x'")
    insert_local(601, 900)

    c.execute("BEGIN")
    for v in QUOTED_INTEGERS:
        c.execute('INSERT INTO "Quoted Table" VALUES(?,?,NULL,NULL)', ("int %d" % v, v))
    c.execute('INSERT INTO "Quoted Table" VALUES(?,NULL,?,NULL)', ("float", 1.5))
    c.execute('INSERT INTO "Quoted Table" VALUES(?,NULL,NULL,?)', ("blob", QUOTED_BLOB))
    c.execute('INSERT INTO "Quoted Table" VALUES(?,NULL,NULL,NULL)', ("null",))
    c.execute("INSERT INTO wr VALUES('a','1'),('b','2')")
    c.execute("COMMIT")

    # 虚拟表放在最后建，不挪动前面各表的页。sqlite_master 里它的 rootpage 为 0；fts5 另建 5 张影子表
    # （vt_data、vt_idx、vt_content、vt_docsize、vt_config，其中 vt_idx 与 vt_config 为 WITHOUT ROWID）。
    # 需要 sqlite3 带 FTS5，Windows 版 Python 自带的 sqlite3 带有。
    c.execute("CREATE VIRTUAL TABLE vt USING fts5(a, b)")
    c.execute("INSERT INTO vt VALUES('alpha', 'beta')")

    # 列约束 INTEGER PRIMARY KEY 后跟 DESC 不是 rowid 别名，id 列存实际值；跟 ASC 仍是别名
    # （https://www.sqlite.org/lang_createtable.html#rowid）。d 的 id 与 rowid 不同，且不按 rowid 的顺序。
    c.execute("CREATE TABLE d(id INTEGER PRIMARY KEY DESC, v TEXT)")
    c.execute("INSERT INTO d(rowid, id, v) VALUES(1, 100, 'x'), (2, 50, 'y')")
    c.execute("CREATE TABLE a(id INTEGER PRIMARY KEY ASC, v TEXT)")
    c.execute("INSERT INTO a(id, v) VALUES(7, 'p'), (9, 'q')")
    assert c.execute("SELECT rowid, id FROM d ORDER BY rowid").fetchall() == [(1, 100), (2, 50)]
    assert c.execute("SELECT rowid, id FROM a ORDER BY rowid").fetchall() == [(7, 7), (9, 9)]
    conn.close()
    check_header(path, 1, 1)


def gen_utf16():
    path, conn = fresh("utf16.db")
    c = conn.cursor()
    c.execute("PRAGMA encoding='UTF-16le'")
    c.execute("PRAGMA page_size=1024")
    c.execute("PRAGMA journal_mode=DELETE")
    c.execute("CREATE TABLE t(a TEXT)")
    c.execute("INSERT INTO t VALUES('utf16')")
    conn.close()
    check_header(path, 1, 2)


def gen_wal():
    path, conn = fresh("wal.db")
    c = conn.cursor()
    c.execute("PRAGMA page_size=1024")
    c.execute("PRAGMA journal_mode=WAL")
    c.execute("CREATE TABLE t(a TEXT)")
    c.execute("INSERT INTO t VALUES('wal')")
    conn.close()  # 正常关闭时检查点写回并删除 -wal；头部读写版本保持为 2
    check_header(path, 2, 1)


# 设计 §2.3 四种样本的 hash、LyricKey 与路径，与 player/kugou/assoc/designsamples_test.go 逐字相同。
SAMPLE_WUMINGZHI = "9d6e3b30c1f04a5e8b7d2c9a6f3e1d08"
SAMPLE_TONGHUA = "20f99236ab4c4d1e9f8a7b6c5d4e3f21"
SAMPLE_SPECTRE = "65aa8965d2e14f3a8c7b6a5948372615"
KEY_WUMINGZHI = "1685c8e7f0a14b2c9d3e4f5a6b7c8d9e"
KEY_TONGHUA = "a2fd05d0e1f2a3b4c5d6e7f8091a2b3c"
ZERO_KEY = "0" * 32
PATH_WUMINGZHI_KRC = (
    "C:\\Users\\user\\Music\\门尼 - 无名指 (The Rust)-bee95e0c1d2e3f4a5b6c7d8e9f0a1b2c-704320507-00000000.krc"
)
PATH_WUMINGZHI_LRC = "1_门尼 - 无名指_(Instrumental)-9d6e3b30c1f04a5e8b7d2c9a6f3e1d08-565331094-00000000.lrc"
PATH_TONGHUA_LRC = "E:\\Downloads\\Lunette\\伴奏\\暗杠 - 童话镇.lrc"
PATH_SPECTRE_KRC = "Alan Walker - Spectre-65aa8965d2e14f3a8c7b6a5948372615-12345678-00000000.krc"

SONG_TABLE_DDL = (
    "CREATE TABLE %s(SongID INTEGER PRIMARY KEY, FilePath nvarchar(260), LyricKey nvarchar(32) NOT NULL DEFAULT '', "
    "MusicFileHash nvarchar(32), FileSize INT)"
)


def gen_kugou_like(name, associated):
    # 仿 KGMusicV3.db 的库，行同 designsamples_test.go 的 designSampleDB(associated)：associated 为假时是
    # 无名指关联之前（设计 §2.3 样本 2），其歌曲条目的 LyricKey 全为 0，LyricInfo 里还没有关联那一行。
    # 另有与样本无关的填充行：LyricInfo 40 行（其中一行带溢出页）、LocalFile 200 行（根为内部页），
    # 以及 500 行、没有 MusicFileHash 与 LyricKey 两列的 ShareFileItems（读库时不应读到它的页）。
    os.makedirs(ASSOC_TESTDATA, exist_ok=True)
    path, conn = fresh(name, ASSOC_TESTDATA)
    c = conn.cursor()
    c.execute("PRAGMA page_size=1024")
    c.execute("PRAGMA journal_mode=DELETE")
    c.execute(LYRIC_INFO_DDL)
    for t in ("LocalFile", "RecentlyPlayedItem", "PlayQueueAssistItem_0"):
        c.execute(SONG_TABLE_DDL % t)
    c.execute("CREATE TABLE ShareFileItems(ID INTEGER PRIMARY KEY, FileHash nvarchar(32), Key nvarchar(32), FileName nvarchar(260))")

    key = KEY_WUMINGZHI if associated else ZERO_KEY
    lyrics = [
        ("5e1c0d2b3a4f5e6d7c8b9a0f1e2d3c4b", PATH_WUMINGZHI_LRC, SAMPLE_WUMINGZHI, 1763200000, None),
        (KEY_TONGHUA, PATH_TONGHUA_LRC, ZERO_KEY, 1763290000, None),
        ("7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d", PATH_SPECTRE_KRC, SAMPLE_SPECTRE, 1763250000, None),
    ]
    if associated:
        lyrics.append((KEY_WUMINGZHI, PATH_WUMINGZHI_KRC, "bee95e0c1d2e3f4a5b6c7d8e9f0a1b2c", 1763280000, None))
    for i in range(40):
        song = md5hex("filler-song-%d" % i)
        lyrics.append((
            md5hex("filler-key-%d" % i), "Singer %02d - Song %02d-%s-%d-00000000.krc" % (i, i, song, 1000 + i),
            song, 1762000000 + i, external_info() if i == 20 else None,
        ))

    c.execute("BEGIN")
    for k, p, h, u, ext in lyrics:
        c.execute(
            "INSERT INTO LyricInfo(Key, KRCID, UpdateTime, LyricPath, SongHash, ExternalInfo) VALUES(?,?,?,?,?,?)",
            (k, 0, u, p, h, ext),
        )

    def song(table, file_path, lyric_key, music_hash):
        c.execute(
            "INSERT INTO %s(FilePath, LyricKey, MusicFileHash, FileSize) VALUES(?,?,?,?)" % table,
            (file_path, lyric_key, music_hash, 1),
        )

    song("LocalFile", "D:\\Music\\门尼 - 无名指_(Instrumental).mp3", key, SAMPLE_WUMINGZHI)
    song("LocalFile", "D:\\QQNT\\Download\\门尼 - 无名指_(Instrumental).mp3", ZERO_KEY, SAMPLE_WUMINGZHI)
    for i in range(200):
        song("LocalFile", "D:\\Music\\Library\\Artist %03d\\Album %03d\\%03d - Track %s.mp3" % (i, i, i, pad(i)[:60]),
             ZERO_KEY if i % 2 else "", md5hex("local-%d" % i))
    song("RecentlyPlayedItem", "D:\\Music\\门尼 - 无名指_(Instrumental).mp3", key, SAMPLE_WUMINGZHI)
    song("PlayQueueAssistItem_0", "D:\\Music\\门尼 - 无名指_(Instrumental).mp3", key, SAMPLE_WUMINGZHI)
    song("PlayQueueAssistItem_0", "", ZERO_KEY, SAMPLE_TONGHUA)
    song("PlayQueueAssistItem_0", "", ZERO_KEY, SAMPLE_TONGHUA)
    song("PlayQueueAssistItem_0", "", KEY_TONGHUA, SAMPLE_TONGHUA)

    c.execute("INSERT INTO ShareFileItems(FileHash, Key, FileName) VALUES(?,?,?)", (SAMPLE_WUMINGZHI, KEY_TONGHUA, "share.kgg"))
    for i in range(1, 500):
        c.execute(
            "INSERT INTO ShareFileItems(FileHash, Key, FileName) VALUES(?,?,?)",
            (md5hex("share-%d" % i), md5hex("share-key-%d" % i), "share %03d.kgg" % i),
        )
    c.execute("COMMIT")
    assert c.execute("SELECT count(*) FROM ShareFileItems").fetchone() == (500,)
    conn.close()
    check_header(path, 1, 1)


if __name__ == "__main__":
    gen_basic()
    gen_utf16()
    gen_wal()
    gen_kugou_like("kugou_like.db", True)
    gen_kugou_like("kugou_like_unassociated.db", False)
