import {
  type ReactNode,
  type Ref,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import type { T } from "@/shared/i18n";
import type { EditorHandle } from "./Editor";
import { plainHTML, plainText } from "./plain";

/**
 * A plain draft's body: a text area in a fixed-width font, loaded from `initial` (plainHTML's form,
 * read at mount only) and reporting every change as plainHTML. No formats: its toolbar is the
 * `attach` slot only, rendered into `toolbarSlot` as Editor's is. It grows with its text; the
 * page's wrapper scrolls.
 */
export function PlainEditor(props: {
  initial: string;
  readOnly: boolean;
  placeholder: string;
  onChange(html: string): void;
  t: T;
  attach?: ReactNode;
  toolbarSlot?: HTMLElement | null;
  ref?: Ref<EditorHandle>;
}) {
  const { readOnly, placeholder, onChange, t, attach, toolbarSlot, ref } = props;
  const [text, setText] = useState(() => plainText(props.initial));
  const area = useRef<HTMLTextAreaElement>(null);
  useImperativeHandle(ref, () => ({ setValue: (html) => setText(plainText(html)) }), []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the height follows the text
  useLayoutEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight}px`;
  }, [text]);
  const toolbar = <div className="-ml-1.5 flex flex-wrap items-center gap-0.5">{attach}</div>;
  return (
    <div className="flex flex-col">
      {toolbarSlot ? createPortal(toolbar, toolbarSlot) : toolbar}
      <textarea
        ref={area}
        aria-label={t("compose.body")}
        value={text}
        readOnly={readOnly}
        placeholder={placeholder}
        rows={8}
        spellCheck
        className="min-h-60 w-full resize-none overflow-hidden bg-transparent font-mono text-sm leading-[1.6] outline-none placeholder:text-muted-foreground"
        onChange={(e) => {
          if (readOnly) return;
          setText(e.target.value);
          onChange(plainHTML(e.target.value));
        }}
      />
    </div>
  );
}
