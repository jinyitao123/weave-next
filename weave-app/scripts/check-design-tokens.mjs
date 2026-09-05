#!/usr/bin/env node
/*
 * Weave 设计 Token 门禁。
 *
 * 规则（token 单一事实来源：src/styles/tokens.css）：
 *   1. 禁止 font-size 字面值（px），只能用 var(--text-*)。
 *   2. 禁止 hex / rgb() / rgba() 色值字面值，只能用语义色 token。
 *   3. 断点只允许三档：736 / 1080 / 1280（见 tokens.css 顶部断点契约）。
 *
 * 豁免：EXEMPT_FILES 中的文件整体豁免（保留基础样式的既有布局数值）。
 * 发现违规即以非零退出并打印清单。
 */
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const SRC = fileURLToPath(new URL("../src", import.meta.url));
const TOKEN_FILE = "styles/tokens.css";

// app.css 保留登录、运行时管理及原语的既有布局；其字面值仍是明确的样式债务。
const EXEMPT_FILES = new Set(["styles/app.css"]);

const ALLOWED_BREAKPOINTS = new Set(["736", "1080", "1280"]);

/** 组件注入式局部变量白名单：由 React style 内联传入，不进入全局 token。 */
const LOCAL_VAR_ALLOWLIST = new Set();

/** tokens.css 中定义的全局 token 集合（供未定义 token 检查使用）。 */
const GLOBAL_TOKENS = new Set(
  readFileSync(new URL(`../src/styles/${TOKEN_FILE.split("/").pop()}`, import.meta.url), "utf8")
    .matchAll(/^\s*(--[a-zA-Z0-9-]+):/gm)
    .map((m) => m[1]),
);

const RULES = [
  {
    name: "font-size-literal",
    pattern: /font-size\s*:\s*(?!var\()[0-9.]+px/g,
    describe: () => "font-size 字面值（应使用 var(--text-*)）",
  },
  {
    name: "color-literal-hex",
    pattern: /#[0-9a-fA-F]{3,8}\b/g,
    describe: () => "hex 色值字面值（应使用语义色 token）",
  },
  {
    name: "color-literal-rgb",
    pattern: /rgba?\s*\(/g,
    describe: () => "rgb()/rgba() 色值字面值（应使用语义色 token）",
  },
  {
    name: "breakpoint-off-contract",
    pattern: /@media[^{]*?(?:width)\s*:\s*([0-9.]+)px/g,
    describe: (m) => `断点 ${m[1]}px 不在契约三档（736/1080/1280）内`,
    filter: (m) => !ALLOWED_BREAKPOINTS.has(m[1]),
  },
];

function* walk(dir) {
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) yield* walk(path);
    else if (/\.(css|tsx?|jsx?)$/.test(entry)) yield path;
  }
}

const violations = [];
for (const path of walk(SRC)) {
  const rel = relative(SRC, path);
  if (rel === TOKEN_FILE || EXEMPT_FILES.has(rel)) continue;
  const lines = readFileSync(path, "utf8").split("\n");
  // 局部 CSS 变量：同文件赋值（`--x:`）或内联 style 注入（tsx 的 "--x":）
  const localDefs = new Set();
  for (const line of lines) {
    for (const m of line.matchAll(/(?:--)([a-zA-Z0-9-]+)(?::|\")/g)) localDefs.add(`--${m[1]}`);
  }
  lines.forEach((line, index) => {
    for (const rule of RULES) {
      rule.pattern.lastIndex = 0;
      let match;
      while ((match = rule.pattern.exec(line)) !== null) {
        if (rule.filter && !rule.filter(match)) continue;
        violations.push(`${rel}:${index + 1}  ${rule.describe(match)}  →  ${line.trim().slice(0, 120)}`);
      }
    }
    for (const m of line.matchAll(/var\((--[a-zA-Z0-9-]+)\)/g)) {
      const token = m[1];
      // 注入式局部变量白名单。
      if (LOCAL_VAR_ALLOWLIST.has(token)) continue;
      if (!GLOBAL_TOKENS.has(token) && !localDefs.has(token)) {
        violations.push(`${rel}:${index + 1}  引用了未定义的 token ${token}（应在 tokens.css 定义或在同文件赋值）  →  ${line.trim().slice(0, 120)}`);
      }
    }
  });
}

if (violations.length) {
  console.error(`设计 Token 门禁：${violations.length} 处违规`);
  for (const v of violations) console.error(`  ${v}`);
  process.exit(1);
}
console.log(`设计 Token 门禁通过（豁免文件：${[...EXEMPT_FILES].join(", ") || "无"}）`);
