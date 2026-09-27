import { useQuery } from "@tanstack/react-query";
import { userServiceClient } from "@/connect";
import type { MemoTemplate } from "@/types/proto/api/v1/user_service_pb";

export const memoTemplateKeys = {
  all: ["memo-templates"] as const,
  list: (parent?: string) => [...memoTemplateKeys.all, parent] as const,
};

export function useMemoTemplates(parent?: string) {
  return useQuery<MemoTemplate[]>({
    queryKey: memoTemplateKeys.list(parent),
    queryFn: async () => {
      if (!parent) return [];
      const { memoTemplates } = await userServiceClient.listMemoTemplates({ parent });
      return memoTemplates;
    },
    enabled: !!parent,
  });
}
