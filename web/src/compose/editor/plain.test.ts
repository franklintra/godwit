// The cases Go's mail.PlainHTML and mail.PlainText keep too.
import shared from "../../../../core/mail/testdata/plain.json?raw";
import { plainHTML, plainText } from "./plain";

describe("plain", () => {
  it("writes and reads the body as Go does", () => {
    const cases = JSON.parse(shared) as { text: string; html: string }[];
    expect(cases.length).toBeGreaterThan(0);
    for (const c of cases) {
      expect(plainHTML(c.text)).toBe(c.html);
      expect(plainText(c.html)).toBe(c.text);
    }
  });

  it("drops the blank lines at either end and reads CRLF", () => {
    expect(plainHTML("\r\n\r\na\r\nb\r\n\r\n")).toBe("<p>a<br>b</p>");
    expect(plainText(plainHTML("  \n\nend\n\n"))).toBe("end");
  });

  it("reads an empty paragraph as none", () => {
    expect(plainText("<p>a</p><p><br></p><p>b</p>")).toBe("a\n\nb");
  });
});
