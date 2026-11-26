import { RiArrowRightSLine } from "@remixicon/react";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import type { Header } from "@/mygo";
import type { T } from "@/shared/i18n";

/**
 * The draft's own header fields (core Draft.Headers), read-only: a program set them, and sending
 * carries them, so the user can look before sending. A row under the fields, folded, with the
 * count; nothing when there are none.
 */
export function Headers({ items, t }: { items: Header[]; t: T }) {
  if (items.length === 0) return null;
  return (
    <Collapsible className="mx-4 border-b-[0.5px] py-2 text-sm">
      <CollapsibleTrigger className="group flex items-center gap-1 text-muted-foreground transition-colors hover:text-foreground">
        <RiArrowRightSLine className="size-3.5 shrink-0 transition-transform group-data-panel-open:rotate-90" />
        {t("compose.headers", String(items.length))}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <p className="pt-1 text-muted-foreground text-xs">{t("compose.headers.help")}</p>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 pt-1.5 font-mono text-xs">
          {items.map((h, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: a name may repeat; the list is read-only
            <div key={i} className="contents">
              <dt className="whitespace-nowrap text-muted-foreground">{h.name}</dt>
              <dd className="min-w-0 select-text break-all">{h.value}</dd>
            </div>
          ))}
        </dl>
      </CollapsibleContent>
    </Collapsible>
  );
}
