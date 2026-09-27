import { create } from "@bufbuild/protobuf";
import { FieldMaskSchema } from "@bufbuild/protobuf/wkt";
import { PencilIcon, PlusIcon, TrashIcon } from "lucide-react";
import { useState } from "react";
import toast from "react-hot-toast";
import ConfirmDialog from "@/components/ConfirmDialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { userServiceClient } from "@/connect";
import useCurrentUser from "@/hooks/useCurrentUser";
import useLoading from "@/hooks/useLoading";
import { useMemoTemplates } from "@/hooks/useMemoTemplateQueries";
import { handleError } from "@/lib/error";
import { MemoTemplate, MemoTemplateSchema } from "@/types/proto/api/v1/user_service_pb";
import { useTranslate } from "@/utils/i18n";
import SettingSection from "./SettingSection";
import SettingTable from "./SettingTable";

const createEmptyTemplate = () => create(MemoTemplateSchema, { name: "", title: "", content: "" });

const TemplatesSection = () => {
  const t = useTranslate();
  const currentUser = useCurrentUser();
  const { data: templates = [], refetch } = useMemoTemplates(currentUser?.name);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [draft, setDraft] = useState<MemoTemplate>(createEmptyTemplate());
  const [deleteTarget, setDeleteTarget] = useState<MemoTemplate | undefined>();
  const requestState = useLoading(false);
  const isEditing = draft.name !== "";

  const openCreateDialog = () => {
    setDraft(createEmptyTemplate());
    setDialogOpen(true);
  };

  const openEditDialog = (template: MemoTemplate) => {
    setDraft(create(MemoTemplateSchema, { name: template.name, title: template.title, content: template.content }));
    setDialogOpen(true);
  };

  const handleSave = async () => {
    const title = draft.title.trim();
    if (!title || !draft.content.trim()) {
      toast.error(t("setting.templates.required-fields"));
      return;
    }
    if (!currentUser) {
      toast.error("User not authenticated");
      return;
    }

    try {
      requestState.setLoading();
      if (isEditing) {
        await userServiceClient.updateMemoTemplate({
          memoTemplate: { name: draft.name, title, content: draft.content },
          updateMask: create(FieldMaskSchema, { paths: ["title", "content"] }),
        });
        toast.success(t("setting.templates.update-success"));
      } else {
        await userServiceClient.createMemoTemplate({
          parent: currentUser.name,
          memoTemplate: { title, content: draft.content },
        });
        toast.success(t("setting.templates.create-success"));
      }
      await refetch();
      requestState.setFinish();
      setDialogOpen(false);
    } catch (error: unknown) {
      handleError(error, toast.error, {
        context: isEditing ? "Update memo template" : "Create memo template",
        onError: () => requestState.setError(),
      });
    }
  };

  const confirmDelete = async () => {
    if (!deleteTarget) return;
    try {
      await userServiceClient.deleteMemoTemplate({ name: deleteTarget.name });
      await refetch();
      toast.success(t("setting.templates.delete-success"));
      setDeleteTarget(undefined);
    } catch (error: unknown) {
      handleError(error, toast.error, { context: "Delete memo template" });
    }
  };

  return (
    <SettingSection
      title={t("setting.templates.title")}
      description={t("setting.templates.description")}
      actions={
        <Button onClick={openCreateDialog}>
          <PlusIcon className="mr-2 size-4" />
          {t("setting.templates.create")}
        </Button>
      }
    >
      <SettingTable
        columns={[
          {
            key: "title",
            header: t("common.name"),
            render: (_, template: MemoTemplate) => <span className="text-foreground">{template.title}</span>,
          },
          {
            key: "content",
            header: t("setting.templates.content"),
            className: "max-w-[420px]",
            render: (_, template: MemoTemplate) => (
              <span className="block max-w-[420px] truncate font-mono text-xs text-muted-foreground" title={template.content}>
                {template.content}
              </span>
            ),
          },
          {
            key: "actions",
            header: "",
            className: "text-right",
            render: (_, template: MemoTemplate) => (
              <>
                <Button variant="ghost" size="sm" onClick={() => openEditDialog(template)} aria-label={t("common.edit")}>
                  <PencilIcon className="size-4" />
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setDeleteTarget(template)} aria-label={t("common.delete")}>
                  <TrashIcon className="size-4 text-destructive" />
                </Button>
              </>
            ),
          },
        ]}
        data={templates}
        emptyMessage={t("setting.templates.no-templates")}
        getRowKey={(template) => template.name}
        variant="info-flow"
      />

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent size="lg">
          <DialogHeader>
            <DialogTitle>{isEditing ? t("setting.templates.edit") : t("setting.templates.create-title")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-4">
            <div className="grid gap-2">
              <Label htmlFor="memo-template-title">{t("setting.templates.name")}</Label>
              <Input
                id="memo-template-title"
                value={draft.title}
                maxLength={100}
                placeholder={t("setting.templates.name-placeholder")}
                onChange={(event) => setDraft((previous) => ({ ...previous, title: event.target.value }))}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="memo-template-content">{t("setting.templates.content")}</Label>
              <Textarea
                id="memo-template-content"
                className="min-h-56 font-mono"
                value={draft.content}
                maxLength={100 * 1024}
                placeholder={t("setting.templates.content-placeholder")}
                onChange={(event) => setDraft((previous) => ({ ...previous, content: event.target.value }))}
              />
            </div>
          </div>
          <DialogFooter>
            <Button variant="ghost" disabled={requestState.isLoading} onClick={() => setDialogOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button disabled={requestState.isLoading} onClick={handleSave}>
              {isEditing ? t("common.save") : t("common.create")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDialog
        open={!!deleteTarget}
        onOpenChange={(open) => !open && setDeleteTarget(undefined)}
        title={t("setting.templates.delete-title", { title: deleteTarget?.title ?? "" })}
        description={t("setting.templates.delete-description")}
        confirmLabel={t("common.delete")}
        cancelLabel={t("common.cancel")}
        onConfirm={confirmDelete}
        confirmVariant="destructive"
      />
    </SettingSection>
  );
};

export default TemplatesSection;
