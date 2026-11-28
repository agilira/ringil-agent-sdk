import { createContext, useContext } from "react";
import en from "./locales/en.json";
import it from "./locales/it.json";
export type Key = keyof typeof en;
export type Language = "en" | "it";
export const I18n = createContext<{
  language: Language;
  t: (key: Key) => string;
}>({ language: "en", t: (k) => en[k] });
export const useI18n = () => useContext(I18n);
export const translate = (language: Language, key: Key) =>
  (language === "it" ? it : en)[key];
