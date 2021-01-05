package api

type SearchIdResponse struct {
	Total int      `json:"total"`
	Data  []string `json:"data"`
}

type MediaDetailResponse struct {
	Data MediaItem `json:"data"`
}

type MediaListResponse struct {
	Total int         `json:"total"`
	Data  []MediaItem `json:"data"`
}

type MediaItem struct {
	FileExtension string            `json:"fileExtension,omitempty"`
	FileSize      int               `json:"fileSize,omitempty"`
	FileName      string            `json:"fileName,omitempty"`
	FolderId      interface{}       `json:"mediaFolderId"`
	ID            string            `json:"id,omitempty"`
	URL           string            `json:"url,omitempty"`
	UploadedAt    string            `json:"uploadedAt,omitempty"`
	CustomFields  map[string]string `json:"customFields,omitempty"`
}

type MediaFolderListResponse struct {
	Total int               `json:"total"`
	Data  []MediaFolderItem `json:"data"`
}

type MediaFolderItem struct {
	Name          string                   `json:"name"`
	ParentId      interface{}              `json:"parentId,omitempty"`
	ID            string                   `json:"id"`
	CreatedAt     string                   `json:"created_at"`
	Configuration MediaFolderConfiguration `json:"configuration"`
}

type MediaFolderConfiguration struct {
	Private bool `json:"private"`
}

type Search struct {
	Includes       map[string][]string `json:"includes,omitempty"`
	Page           int64               `json:"page,omitempty"`
	Limit          int64               `json:"limit,omitempty"`
	IDs            []string            `json:"ids,omitempty"`
	Filter         []SearchFilter      `json:"filter,omitempty"`
	PostFilter     []SearchFilter      `json:"postFilter,omitempty"`
	Sort           []SearchSort        `json:"sort,omitempty"`
	Term           string              `json:"term,omitempty"`
	TotalCountMode int                 `json:"totalCountMode,omitempty"`
}

type SearchFilter struct {
	Type     string         `json:"type"`
	Operator string         `json:"operator,omitempty"`
	Field    string         `json:"field,omitempty"`
	Value    interface{}    `json:"value"`
	Queries  []SearchFilter `json:"queries,omitempty"`
}

type SearchSort struct {
	Direction      string `json:"order"`
	Field          string `json:"field"`
	NaturalSorting bool   `json:"naturalSorting"`
}

type SearchResponse struct {
	Total        int64       `json:"total"`
	Data         interface{} `json:"data"`
	Aggregations interface{} `json:"aggregations"`
}
