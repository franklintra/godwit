import { RiArrowRightSLine, RiMailCheckLine } from "@remixicon/react";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import type { Header } from "@/mygo";
import type { T } from "@/shared/i18n";

/**
 * What the draft sends beyond its fields, read-only: a program set it, and sending carries it, so
 * the user can look before sending. A read receipt request shows as a row; the draft's own header
 * fields (core Draft.Headers) as a folded row with their count. Nothing when there is neither.
 */
export function Headers({
  items,
  readReceipt,
  t,
}: {
  items: Header[];
  readReceipt: boolean;
  t: T;
}) {
  return (
    <>
      {readReceipt && (
        <div className="mx-4 flex items-center gap-1.5 border-b-[0.5px] py-2 text-sm">
          <RiMailCheckLine className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="text-muted-foreground">{t("compose.read_receipt")}</span>
          <span>{t("compose.read_receipt.on")}</span>
        </div>
      )}
      {items.length > 0 && (
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
      )}
    </>
  );
}
