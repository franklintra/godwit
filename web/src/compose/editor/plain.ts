// A plain draft's body as the page edits it: text, kept in the draft as Go's mail.PlainHTML writes
// it (lines a blank line apart are paragraphs, a line break inside one is a <br>, each more blank
// line one more <br> at the end of the paragraph before). Go hands the page that form only
// (webui pageBody), so these two read and write it, nothing more.

// As Go's html.EscapeString, so a body the page writes is the one Go would.
const esc = (s: string) =>
  s
    .replace(/&/g, "&amp;")
    .replace(/'/g, "&#39;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&#34;");

const blank = (l: string) => l.trim() === "";

/** The compose HTML of text, as mail.PlainHTML writes it; blank lines at either end go. */
export function plainHTML(text: string): string {
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  while (lines.length && blank(lines[0])) lines.shift();
  while (lines.length && blank(lines[lines.length - 1])) lines.pop();
  let out = "";
  for (let i = 0; i < lines.length; ) {
    const para: string[] = [];
    for (; i < lines.length && lines[i] !== ""; i++) para.push(esc(lines[i]));
    let more = 0;
    for (; i < lines.length && lines[i] === ""; i++) more++;
    out += `<p>${para.join("<br>")}${more > 1 ? "<br>".repeat(more) : ""}</p>`;
  }
  return out;
}

/** The text of plainHTML's form: mail.PlainText for it. */
export function plainText(html: string): string {
  const body = new DOMParser().parseFromString(html, "text/html").body;
  const paras: string[] = [];
  for (const block of body.childNodes) {
    const lines = [""];
    for (const n of block.childNodes) {
      if (n.nodeName === "BR") lines.push("");
      else lines[lines.length - 1] += n.textContent ?? "";
    }
    if (block.nodeType === Node.TEXT_NODE) lines[0] = block.textContent ?? "";
    // The <br> that holds a paragraph's last line open is no line.
    if (block.lastChild?.nodeName === "BR") lines.pop();
    if (lines.length === 1 && lines[0] === "") continue; // an empty paragraph only parts the others
    paras.push(lines.join("\n"));
  }
  return paras.join("\n\n");
}
