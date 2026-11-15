# arxiv-go

arXiv 客户端,三个工程 + 一个共享包,用 SQLite 做任务队列。

## 结构

- `downloader/` 下载工程:不停从 SQLite 读任务执行网络请求;搜索结果存 XML,PDF 一律断点续传
- `submitter/` 触发工程:把任务写进库拿到 id,轮询直到成功
- `taskdb/` 共享包:建表/提交/查询(GORM + glebarez/sqlite 纯 Go 驱动)
- `itest/` 集成测试:编译并运行 downloader + submitter,完整跑通搜索、获取、下载、去重、失败路径

## 编译

```sh
go build -o bin/downloader ./downloader
go build -o bin/submitter  ./submitter
```

## SQLite 放哪、怎么建

**不需要手动建库**:首次运行(downloader 或 submitter 任一)会自动建表(GORM AutoMigrate)。

默认路径:**queue.sqlite 就在二进制旁边**——即二进制所在目录下的 `queue.sqlite`。
两个二进制放同一目录,自然共用同一个库:

```
bin/
├── downloader
├── submitter
├── queue.sqlite          ← 首次运行自动出现
├── queue.sqlite-wal      ← WAL 伴随文件
├── queue.sqlite-shm
└── results/              ← 结果文件 task-<id>.xml / task-<id>.pdf
```

要放别处就显式传 `--db /任意/路径/queue.sqlite`(downloader 和 submitter 指向同一个文件)。

## 运行

```sh
./bin/downloader                                  # 常驻消费;--min-interval 3s --timeout 60s --max-attempts 8
./bin/submitter search 'ti:"electron thermal conductivity"' --max 5
./bin/submitter search --cat cs.LG --from 2026-11-01 --to 2026-11-07
./bin/submitter get 1607.03037
./bin/submitter get 1607.03037 --json
./bin/submitter download 1607.03037               # 自动解析最新版本号,输出 pdf 路径
./bin/submitter daily 2026-11-14                  # 下载某天全部新论文的 PDF
```

submitter 全局参数:`--db` `--timeout 10m` `--ttl 5m`(搜索结果新鲜窗口)`--no-wait`。

## 每小时定时下载(cron 驱动,submitter 立即返回)

工程本身不带调度,定时交给 cron/launchd:**downloader 常驻慢慢消费,submitter 只投递、立即返回,不等下载**。

downloader 常驻:

```sh
nohup ./downloader >> downloader.log 2>&1 &     # 或 launchd KeepAlive
```

crontab 每小时整点投递(当天新论文 PDF 全部入队,submitter 秒回):

```cron
0 * * * * cd /path/to/bin && ./submitter daily $(date +\%F) --no-wait >> cron.log 2>&1
```

已有外部扫描流程(如逐分类窄查询)时,直接喂**带版本号的 id**,零额外元数据请求:

```sh
./submitter download 2611.07673v1 2611.07674v1 --no-wait
```

`--no-wait` 语义(命令后缀或全局前缀均可):

- search / get:提交后打印 `task <id>` 即退出
- download:打印 `<paper> task <id>`(无版本号的仍会先查一次元数据,秒级;带 vN 的直接入队)
- daily:等一次搜索拿到清单(必须,才能枚举 PDF),入队全部后打印 `queued N` 即退出,不等任何 PDF

结果落盘在 `results/task-<id>.xml|.pdf`;查进度:

```sh
sqlite3 queue.sqlite 'select status,count(*) from tasks group by status'
```

同一台机器若还有其他 arXiv 抓取任务,downloader 建议 `--min-interval 5s` 起步更稳(共享配额)。

## 队列语义

- `(kind, url)` 唯一:相同任务去重;done 且未过期直接复用(search 默认 5 分钟,get 24 小时,版本化 PDF 永久)
- 串行并发 1;上一个请求**完成后再空 3s** 才发下一个(比 arXiv ToU 更保守)
- 失败自动回队重试,达到 `--max-attempts` 转 failed 并记录 last_error;进程重启后 running 状态自动复位
- 下载只打版本化 URL(`arxiv.org/pdf/<id>vN`),作者更新新版本不影响已下载内容
- 断点续传:`Range: bytes=N-` 续传,206 追加 / 200 重写 / 416 删本地重来;磁盘上的半成品文件就是进度

## 测试

```sh
go run ./itest          # 本地 mock arXiv,含断点续传演练(故意掐断再续传)
go run ./itest -live    # 打真实 arXiv
```
