#!/usr/bin/env python3
"""生成 internal/ruleset/catalog.go（内置规则集清单）。

上游 blackmatrix7/ios_rule_script 只有规则文件、没有分类信息，所以清单在这里维护：

1. **不收录中国相关**。这个清单的用途是「只允许访问特定服务」的白名单，
   收录国内站点没有意义——这些站点本来就不需要走代理，放进来只会让面板变长。
   通用清单（China / ChinaIPs / ChinaMax …）与具体大陆服务一并排除；
   港澳台服务保留（在大陆访问它们通常需要代理）。
2. **AI 相关归并成一项**。上游把 OpenAI / Claude / Gemini / Copilot 拆成 8 个文件，
   对使用者来说「我只要放行 AI 站点」，拆开选既费事又容易漏，合并成一个 AI 项。
3. 其余按首字母分组，高频项补中文标签。

用法：
    python3 scripts/gen-ruleset-catalog.py > internal/ruleset/catalog.go
"""

import json
import pathlib
import subprocess
import sys
import time
import urllib.request

UPSTREAM_REPO = "blackmatrix7/ios_rule_script"
UPSTREAM_REF = "master"
UPSTREAM_DIR = "rule/Surge"

# 通用清单、拦截类、策略类：都不是「某个服务」，放进白名单没有意义。
EXCLUDE_EXACT = {
    # 名字含单引号，不满足 sbx 对规则集名的保守字符集要求（名字会拼进
    # 文件路径与下载 URL，只允许 [A-Za-z0-9_-]）。
    "Assassin'sCreed",
    # 通用/中国清单
    "China", "ChinaASN", "ChinaDNS", "ChinaIPs", "ChinaIPsBGP", "ChinaMax",
    "ChinaMaxNoIP", "ChinaMaxNoMedia", "ChinaMedia", "ChinaMobile", "ChinaNews",
    "ChinaNoMedia", "ChinaTest", "ChinaTelecom", "ChinaUnicom", "IPTVMainland",
    # 拦截/隐私/策略类
    "AdGuardSDNSFilter", "Advertising", "AdvertisingLite", "AdvertisingMiTV",
    "AdvertisingTest", "BlackList", "Direct", "EasyPrivacy", "Global",
    "GlobalMedia", "Hijacking", "MIUIPrivacy", "Privacy", "Proxy", "ProxyLite",
    # 纯工具类
    "NTPService",
}

# 中国大陆服务：银行、运营商、互联网大厂、App、媒体……
EXCLUDE_MAINLAND = {
    # 数字与拼音站点
    "115", "12306", "17173", "178", "17zuoye", "360", "36kr", "3Type", "3dm",
    "4399", "4Paradigm", "51Job", "51nod", "56", "58TongCheng", "6JianFang", "8btc",
    # A
    "AcFun", "Afdian", "AiQiCha", "AirChina", "Ali213", "AliPay", "Alibaba",
    "AmazonCN", "AnTianKeJi", "Anjuke", "Atomdata", "AppleDaily",
    # B
    "BOC", "BOCOM", "BaiDuTieBa", "BaiFenDian", "BaiShanYunKeJi", "Baidu",
    "BaoFengYingYin", "BesTV", "BianFeng", "BiliBili", "BiliBiliIntl", "Blued",
    "BoXun", "Bootcss", "ByteDance",
    # C
    "CAS", "CCB", "CCTV", "CEB", "CETV", "CGB", "CIBN", "CKJR", "CMB", "CNKI",
    "CNNIC", "CSDN", "CaiNiao", "CaiXinChuanMei", "Camera360", "ChengTongWangPan",
    "ChuangKeTie", "ChunYou", "Coolapk",
    # D
    "DaMai", "DanDanZan", "Dandanplay", "DangDang", "Dedao", "DiDi",
    "DiLianWangLuo", "DiSiFanShi", "DianCeWangKe", "DingTalk", "DingXiangYuan",
    "DouBan", "DouYin", "Douyu", "DuoWan",
    # E
    "EastMoney", "Eleme",
    # F
    "FanFou", "FangZhengDianZi", "FeiZhu", "FengHuangWang", "FengXiaWangLuo", "Funshion",
    # G
    "GaoDe", "Geely", "Gitee", "GovCN", "GuiGuDongLi",
    # H
    "HWTV", "HaiNanHangKong", "HanYi", "HeMa", "HibyMusic", "Hpplay", "HuYa",
    "HuaShuTV", "HuanJu", "Huawei", "HunanTV", "Hupu",
    # I
    "ICBC", "iFlytek", "iQIYI", "iQIYIIntl",
    # J
    "JianGuoYun", "JianShu", "JinJiangWenXue", "JingDong", "JueJin",
    # K
    "Keep", "Kingsoft", "KouDaiShiShang", "Ku6", "KuKeMusic", "KuaiDi100",
    "KuaiShou", "KuangShi", "KugouKuwo",
    # L
    "LanZouYun", "LeJu", "LeTV", "LuDaShi", "LvMiLianChuang",
    # M
    "MOMOShop", "MOOMusic", "Maocloud", "MeiTu", "MeiTuan", "MeiZu", "MiWu",
    "Migu", "MingLueZhaoHui", "Mogujie", "Mojitianqi",
    # N
    "NGA", "NGAA",
    # O
    "OPPO", "OuPeng",
    # P
    "PPTV", "PSBC", "Picacg", "Picsee", "PikPak", "Pinduoduo", "PingAn", "PotatoChat",
    # Q
    "QiNiuYun", "QingCloud", "Qyyjt",
    # R
    "RuanMei",
    # S
    "SFExpress", "SMZDM", "ShangHaiJuXiao", "Shanling", "ShenMa", "ShiJiChaoXing",
    "ShiNongZhiKe", "Sina", "Sohu", "SouFang", "SuNing", "SuiShiChuanMei",
    # T
    "TCL", "TaiKang", "TapTap", "Teambition", "Tencent", "TencentVideo",
    "TianTianKanKan", "TianWeiChengXin", "TianYaForum", "TigerFintech", "TongCheng",
    # U
    "U17", "UC", "UCloud", "UPYun", "UnionPay",
    # V
    "Vancl", "VikACG", "VipShop", "Vivo",
    # W
    "WanKaHuanJu", "WanMeiShiJie", "Wanfang", "WangSuKeJi", "WangXinKeJi", "WeChat",
    "WeType", "WeiZhiYunDong", "Weibo", "WenJuanXing", "WiFiMaster", "Wteam",
    # X
    "XiamiMusic", "XianYu", "XiaoGouKeJi", "XiaoHongShu", "XiaoMi", "XiaoYuanKeJi",
    "XieCheng", "XingKongWuXian", "XueErSi", "XueQiu", "Xunlei",
    # Y
    "YYeTs", "YiChe", "YiXiaKeJi", "YiZhiBo", "YouMengChuangXiang", "YouZan",
    "Youku", "YuanFuDao",
    # Z
    "ZDNS", "ZhangYue", "ZhiYinManKe", "ZhiYunZhong", "Zhihu", "ZhihuAds",
    "ZhongGuoShiHua", "ZhongWeiShiJi", "ZhongXingTongXun", "ZhongYuanYiShang",
    "ZhuanZhuan", "zhanqi",
}

EXCLUDE = EXCLUDE_EXACT | EXCLUDE_MAINLAND

# AI 相关归并成一项。这几个文件在上游是各自独立的，合并后由 sbx 下载并去重。
AI_ITEM_NAME = "AI"
AI_ITEM_LABEL = "AI（OpenAI / ChatGPT / Claude / Gemini / Copilot 等）"
AI_SOURCES = [
    "OpenAI", "Claude", "Anthropic", "Gemini",
    "BardAI", "Copilot", "Civitai", "aiXcoder",
]

# 高频项单独成组置顶。全量清单里光 A 组就有五十多项，把「AI」「Netflix」这些
# 藏进去等于让人每次都得搜一次。这些项从字母组里移出，不会重复出现。
COMMON_GROUP = "常用"
COMMON = [
    "Google", "YouTube", "Microsoft", "Apple", "GitHub", "Cloudflare", "Wikipedia",
    "AI",
    "TikTok", "Twitter", "Facebook", "Instagram", "Telegram", "Reddit", "Discord",
    "Whatsapp", "Pinterest",
    "Netflix", "Disney", "HBO", "Spotify", "PrimeVideo", "Twitch", "YouTubeMusic",
    "Steam", "Epic", "Nintendo", "PlayStation", "Xbox", "Blizzard",
    "Notion", "Figma", "Dropbox", "Speedtest",
    "PayPal", "Binance", "OKX", "Crypto",
]

# 高频项的中文标签，其余项直接用规则集名。
LABELS = {
    "Google": "Google（搜索、Gmail、地图）",
    "YouTube": "YouTube",
    "YouTubeMusic": "YouTube Music",
    "Microsoft": "微软（含 Office、OneDrive）",
    "Apple": "Apple（含 App Store、iCloud）",
    "AppStore": "App Store",
    "iCloud": "iCloud",
    "GitHub": "GitHub",
    "GitLab": "GitLab",
    "Cloudflare": "Cloudflare",
    "Amazon": "Amazon",
    "AmazonPrimeVideo": "Amazon Prime Video",
    "TikTok": "TikTok",
    "Twitter": "X / Twitter（含 Grok）",
    "Facebook": "Facebook",
    "Instagram": "Instagram",
    "Telegram": "Telegram",
    "Reddit": "Reddit",
    "Discord": "Discord",
    "Whatsapp": "WhatsApp",
    "Pinterest": "Pinterest",
    "LinkedIn": "LinkedIn",
    "Threads": "Threads",
    "Snap": "Snapchat",
    "Netflix": "Netflix",
    "Disney": "Disney+",
    "HBO": "HBO / Max",
    "Spotify": "Spotify",
    "Twitch": "Twitch",
    "SoundCloud": "SoundCloud",
    "Emby": "Emby（自建媒体库）",
    "Steam": "Steam",
    "Epic": "Epic Games",
    "Blizzard": "暴雪",
    "Nintendo": "任天堂",
    "Xbox": "Xbox",
    "PlayStation": "PlayStation",
    "EA": "EA",
    "Riot": "Riot Games",
    "Ubisoft": "Ubisoft",
    "HoYoverse": "米哈游（原神 / 星穹铁道）",
    "Notion": "Notion",
    "Figma": "Figma",
    "Slack": "Slack",
    "Teams": "Microsoft Teams",
    "Docker": "Docker",
    "Vercel": "Vercel",
    "Npmjs": "npm",
    "Oracle": "Oracle Cloud",
    "PayPal": "PayPal",
    "Stripe": "Stripe",
    "Binance": "Binance",
    "OKX": "OKX",
    "Crypto": "加密货币相关",
    "Cryptocurrency": "加密货币相关",
    "Wikipedia": "维基百科",
    "Duckduckgo": "DuckDuckGo",
    "Speedtest": "Speedtest 测速",
    "OneDrive": "OneDrive",
    "Dropbox": "Dropbox",
    "MEGA": "MEGA 网盘",
    "Protonmail": "Proton 邮箱",
    "Zoom": "Zoom",
    "Salesforce": "Salesforce",
    "IBM": "IBM",
    "Intel": "Intel",
    "Nvidia": "NVIDIA",
    "AMD": "AMD",
    "Adobe": "Adobe",
    "Vercel": "Vercel",
    "Oreilly": "O'Reilly",
    "PlayStation": "PlayStation",
}


def list_upstream_names():
    """列出上游 rule/Surge 下的全部规则集名（走 git trees API，一次拿全）。

    trees API 的响应有几 MB，实测会被中途掐断，所以要重试；读的时候一次性
    read() 而不是迭代，避免 chunked 传输的 IncompleteRead。
    """
    url = ("https://api.github.com/repos/%s/git/trees/%s?recursive=1"
           % (UPSTREAM_REPO, UPSTREAM_REF))
    prefix = UPSTREAM_DIR + "/"
    last = None
    for attempt in range(5):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "sbx-catalog-gen"})
            with urllib.request.urlopen(req, timeout=120) as resp:
                tree = json.loads(resp.read())
            names = set()
            for entry in tree.get("tree", []):
                path = entry.get("path", "")
                if entry.get("type") != "tree" or not path.startswith(prefix):
                    continue
                rest = path[len(prefix):]
                if "/" in rest or not rest:
                    continue
                names.add(rest)
            if names:
                return sorted(names)
            last = RuntimeError("上游目录为空")
        except Exception as e:  # noqa: BLE001 - 网络抖动都重试
            last = e
        time.sleep(2 * (attempt + 1))
    raise SystemExit("获取上游清单失败（已重试 5 次）: %s" % last)


def group_of(name):
    first = name[0].upper()
    return "0-9" if first.isdigit() else first


def go_string(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def main():
    out_path = None
    argv = sys.argv[1:]
    if argv:
        if len(argv) == 2 and argv[0] == "-o":
            out_path = pathlib.Path(argv[1])
        else:
            sys.exit("用法: gen-ruleset-catalog.py [-o 输出文件]（缺省写到 stdout）")

    names = list_upstream_names()
    if not names:
        sys.exit("未能从上游取到规则集清单")

    missing_ai = [n for n in AI_SOURCES if n not in names]
    if missing_ai:
        sys.exit("AI 归纳集引用了上游不存在的规则集: %s" % ", ".join(missing_ai))

    missing_common = [n for n in COMMON if n not in names and n != AI_ITEM_NAME]
    if missing_common:
        sys.exit("常用组引用了上游不存在的规则集: %s" % ", ".join(missing_common))

    kept = [n for n in names
            if n not in EXCLUDE and n not in AI_SOURCES and n not in COMMON]

    groups = {COMMON_GROUP: list(COMMON)}
    for n in kept:
        groups.setdefault(group_of(n), []).append(n)
    for name, items in groups.items():
        if name != COMMON_GROUP:
            items.sort()

    out = []
    out.append("// Code generated by scripts/gen-ruleset-catalog.py; DO NOT EDIT.\n")
    out.append("\npackage ruleset\n")
    out.append("""
// catalog 是内置的可选清单：上游全部规则集，去掉中国相关与拦截/策略类，
// 再把 AI 相关归并成一项。名字必须与上游 rule/Surge 下的目录名完全一致
// （大小写敏感），Sources 为空表示与 Name 同名。
//
// 这份清单是白名单的第一道闸：只有清单内的名字才会被拼进文件路径与
// sing-box 配置，所以不存在路径穿越，也不会把任意 URL 写进配置。
var catalog = []Group{""")

    for g in [COMMON_GROUP] + sorted(k for k in groups if k != COMMON_GROUP):
        out.append('\t{Name: %s, Items: []Item{' % go_string(g))
        for name in groups[g]:
            if name == AI_ITEM_NAME:
                src = ", ".join(go_string(s) for s in AI_SOURCES)
                out.append('\t\t{Name: %s, Label: %s, Sources: []string{%s}},'
                           % (go_string(AI_ITEM_NAME), go_string(AI_ITEM_LABEL), src))
            else:
                out.append('\t\t{Name: %s, Label: %s},'
                           % (go_string(name), go_string(LABELS.get(name, name))))
        out.append("\t}},")
    out.append("}\n")

    # 排除名单也生成出来，供测试断言「清单里确实没有这些项」。
    out.append("// catalogExcluded 是生成清单时被排除的名字，仅用于测试守卫。")
    out.append("var catalogExcluded = []string{")
    for name in sorted(EXCLUDE):
        out.append("\t%s," % go_string(name))
    out.append("}\n")

    text = "\n".join(out)
    if out_path is None:
        sys.stdout.write(text)
        return
    # 先写临时文件再替换：生成中途失败（比如网络断了）不会把已有文件截空。
    tmp = out_path.with_name(out_path.name + ".tmp")
    tmp.write_text(text, encoding="utf-8")
    tmp.replace(out_path)
    kept_count = sum(len(v) for v in groups.values())
    print("已写入 %s：%d 个分组 / %d 个规则集" % (out_path, len(groups), kept_count),
          file=sys.stderr)


if __name__ == "__main__":
    main()
