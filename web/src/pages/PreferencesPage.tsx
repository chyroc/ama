import { ThemedComponent } from "@/components/ui/ThemedComponent";
import { ClassicPreferencesPage } from "@/components/preferences/ClassicPreferencesPage";
import { AzurePreferencesPage } from "@/components/preferences/AzurePreferencesPage";

/** Preference page semantic contract; each theme owns its complete presentation. */
export function PreferencesPage() {
  return <ThemedComponent classic={() => <ClassicPreferencesPage />} azure={() => <AzurePreferencesPage />} />;
}
