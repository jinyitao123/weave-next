import DOMPurify from "dompurify";
import { marked } from "marked";

export function MarkdownText({ text }: { text: string }) {
  const html = DOMPurify.sanitize(marked.parse(text, { async: false, gfm: true, breaks: true }));
  return <div className="markdown-text" dangerouslySetInnerHTML={{ __html: html }} />;
}
