import { RiLockLine, RiLockUnlockLine, RiShieldCheckLine } from "@remixicon/react";
import { Button } from "@/components/button";
import { type PopupMenu, popupUnder, separator } from "@/components/wit-menu";
import type { OpenPGP as State } from "@/mygo";
import type { T } from "@/shared/i18n";

const modes = ["off", "auto", "on"] as const;

/**
 * Whether the draft goes out encrypted, as the button shows it: "on" and "auto" with every
 * recipient's key at hand do; "auto" with one missing goes signed only.
 */
export function encrypts(s: State, missing: string[], recipients: number): boolean {
  if (s.encrypt === "on") return true;
  return s.encrypt === "auto" && recipients > 0 && missing.length === 0;
}

/**
 * The OpenPGP control in the bottom bar (ADR 0050): what the draft will be, signed and encrypted,
 * and a native menu to change it; then the recipients whose key is missing, while encryption is
 * asked for. Hidden while the sending account has no OpenPGP key.
 */
export function OpenPGP({
  state,
  missing,
  recipients,
  frozen,
  onChange,
  menu,
  t,
}: {
  state: State;
  /** The recipients with no valid key in the user's keyring. */
  missing: string[];
  /** How many recipients the draft has. */
  recipients: number;
  frozen: boolean;
  onChange(sign: boolean, encrypt: string): void;
  menu: PopupMenu;
  t: T;
}) {
  if (!state.available) return null;
  const encrypted = encrypts(state, missing, recipients);
  const label = [
    state.sign ? t("compose.openpgp.signed") : "",
    encrypted ? t("compose.openpgp.encrypted") : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const open = async (e: React.MouseEvent<HTMLElement>) => {
    const id = await popupUnder(menu, e.currentTarget, [
      { id: "sign", label: t("compose.openpgp.sign"), checked: state.sign },
      separator,
      ...modes.map((m) => ({
        id: `encrypt:${m}`,
        label: t(`compose.openpgp.encrypt_${m}`),
        checked: state.encrypt === m,
        radio: true,
      })),
    ]);
    if (id === "sign") onChange(!state.sign, state.encrypt);
    else if (id.startsWith("encrypt:")) onChange(state.sign, id.slice("encrypt:".length));
  };
  const warn = state.encrypt !== "off" && missing.length > 0;
  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        disabled={frozen}
        aria-label={t("compose.openpgp")}
        title={t("compose.openpgp")}
        onClick={open}
        data-openpgp
      >
        {encrypted ? <RiLockLine /> : state.sign ? <RiShieldCheckLine /> : <RiLockUnlockLine />}
        {label || t("compose.openpgp")}
      </Button>
      {warn && (
        <span
          className="text-(color:--warning-text) text-xs"
          title={missing.join(", ")}
          data-openpgp-missing
        >
          {t(
            state.encrypt === "on" ? "compose.openpgp.missing_on" : "compose.openpgp.missing_auto",
            missing.join(", "),
          )}
        </span>
      )}
    </>
  );
}
